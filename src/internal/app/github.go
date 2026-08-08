package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	githubAPI     = "https://api.github.com"
	githubTimeout = 20 * time.Second
	githubRetries = 3
	githubLimit   = 1 << 20
	// Сколько последних issue просматриваем в поиске своего маркера. Дубль
	// ищется сразу после потерянного ответа, поэтому нужный тикет лежит в самом
	// начале списка.
	githubScan = 30
)

// GitHub - клиент Issues API. Один сервис, четыре запроса: SDK потянул бы
// чужую модель ошибок и повторов ради того же кода.
type GitHub struct {
	token string
	http  *http.Client
	log   *slog.Logger
}

func NewGitHub(token string, log *slog.Logger) *GitHub {
	return &GitHub{token: token, http: &http.Client{Timeout: githubTimeout}, log: log}
}

// Метки проекта. Статус тикета живёт меткой и остаётся единственным источником
// истины: сервис их только заводит и читает.
var baseLabels = []struct{ Name, Color, Desc string }{
	{"type:bug", "d73a4a", "Сервис ведёт себя не так, как ожидали"},
	{"type:feature", "a2eeef", "Нужно то, чего в сервисе нет"},
	{"type:question", "d876e3", "Нужен ответ, а не изменение в коде"},
	{"status:new", "0e8a16", "Заведён, к работе не приступали"},
	{"incomplete", "fbca04", "Контракт готовности недобран, пробелы в теле"},
}

const authorLabelColor = "ededed"

// CheckAccess - видит ли токен Issues проекта. Fine-grained PAT выдаётся на
// конкретные репозитории, поэтому право на второй проект не следует из права на
// первый, и проверять надо каждый.
//
// Смотрим именно Issues, а не сам репозиторий: доступ к репозиторию есть и у
// токена, которому Issues не выданы вовсе. Права на запись эта проверка всё
// равно не доказывает - её даёт только первый созданный тикет.
func (g *GitHub) CheckAccess(ctx context.Context, p Project) error {
	path := fmt.Sprintf("/repos/%s/%s/issues?per_page=1", p.Owner, p.Repo)
	_, err := g.call(ctx, http.MethodGet, path, nil)
	return err
}

// EnsureLabels заводит недостающие метки проекта. Автосоздание метки при
// создании issue документацией не обещано, поэтому шаг явный.
func (g *GitHub) EnsureLabels(ctx context.Context, p Project) error {
	for _, l := range baseLabels {
		if err := g.createLabel(ctx, p, l.Name, l.Color, l.Desc); err != nil {
			return err
		}
	}
	g.log.Info("labels_created", "project", p.Slug, "labels", len(baseLabels))
	return nil
}

// createLabel: существующая метка возвращает 422, и это не ошибка - именно
// такого состояния мы и добивались.
func (g *GitHub) createLabel(ctx context.Context, p Project, name, color, desc string) error {
	body, err := json.Marshal(map[string]string{"name": name, "color": color, "description": desc})
	if err != nil {
		return fmt.Errorf("build label %s: %w", name, err)
	}

	_, err = g.call(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/labels", p.Owner, p.Repo), body)
	var apiErr *githubError
	if errors.As(err, &apiErr) && apiErr.status == http.StatusUnprocessableEntity {
		return nil
	}
	return err
}

// CreateIssue заводит тикет и возвращает его номер и ссылку.
func (g *GitHub) CreateIssue(ctx context.Context, p Project, title, body string, labels []string) (int, string, error) {
	payload, err := json.Marshal(map[string]any{"title": title, "body": body, "labels": labels})
	if err != nil {
		return 0, "", fmt.Errorf("build issue: %w", err)
	}

	// Единственный неидемпотентный запрос сервиса, и повторов у него нет.
	// Оборванный ответ на успешный POST означает, что тикет уже создан: слепой
	// повтор дал бы второй. Повторяет очередь, а она перед этим ищет маркер.
	raw, _, err := g.send(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues", p.Owner, p.Repo), payload)
	if err != nil {
		return 0, "", err
	}

	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, "", fmt.Errorf("decode issue: %w", err)
	}
	if out.Number == 0 {
		return 0, "", errors.New("github returned issue without number")
	}
	return out.Number, out.HTMLURL, nil
}

// FindIssue ищет свой маркер среди последних тикетов репозитория. Нужен на
// повторе: ответ на успешный запрос мог потеряться, и без проверки обращение
// уехало бы в GitHub вторым тикетом.
//
// Список, а не поиск: индекс search обновляется с задержкой, и только что
// созданный issue он не покажет, а список отдаёт его сразу.
func (g *GitHub) FindIssue(ctx context.Context, p Project, marker string) (int, string, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues?state=all&per_page=%d&sort=created&direction=desc",
		p.Owner, p.Repo, githubScan)
	raw, err := g.call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, "", err
	}

	var issues []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(raw, &issues); err != nil {
		return 0, "", fmt.Errorf("decode issue list: %w", err)
	}
	for _, issue := range issues {
		if strings.Contains(issue.Body, marker) {
			return issue.Number, issue.HTMLURL, nil
		}
	}
	return 0, "", nil
}

// githubError несёт код ответа: 422 на метке означает «уже есть», и отличить
// его от настоящего отказа можно только по статусу.
type githubError struct {
	status  int
	message string
}

func (e *githubError) Error() string { return fmt.Sprintf("github status %d: %s", e.status, e.message) }

func (g *GitHub) call(ctx context.Context, method, path string, body []byte) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		raw, retry, err := g.send(ctx, method, path, body)
		if err == nil {
			return raw, nil
		}
		if !retry || attempt == githubRetries {
			return nil, err
		}

		g.log.Warn("github_retry", "path", path, "attempt", attempt+1, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * time.Second):
		}
	}
}

// send делает одну попытку. Второе значение - повторять ли: 429, 5xx и
// вторичный лимит (403 с Retry-After) да, остальные 4xx нет.
func (g *GitHub) send(ctx context.Context, method, path string, body []byte) (json.RawMessage, bool, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, githubAPI+path, reader)
	if err != nil {
		return nil, false, fmt.Errorf("build github request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := g.http.Do(req)
	if err != nil {
		// Ответа не было, значит и запрос мог не отработать: повтор безопасен,
		// от дубля защищает поиск маркера.
		return nil, true, fmt.Errorf("github %s: %w", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, githubLimit))
	if err != nil {
		return nil, true, fmt.Errorf("read github body: %w", err)
	}
	if resp.StatusCode >= 300 {
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 ||
			(resp.StatusCode == http.StatusForbidden && resp.Header.Get("Retry-After") != "")
		return nil, retry, &githubError{status: resp.StatusCode, message: githubMessage(raw)}
	}
	return raw, false, nil
}

func githubMessage(raw []byte) string {
	var out struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "non-json body"
	}
	return cut(out.Message)
}

// Publisher - последний шаг обращения: подтверждённое саммари становится issue.
type Publisher struct {
	cases *Cases
	gh    *GitHub
	rules Contract
	log   *slog.Logger
}

func NewPublisher(cases *Cases, gh *GitHub, rules Contract, log *slog.Logger) *Publisher {
	return &Publisher{cases: cases, gh: gh, rules: rules, log: log}
}

// Run создаёт issue по обращению. Идемпотентен трижды: по ключу работы, по уже
// записанному номеру issue и по маркеру в теле тикета.
func (p *Publisher) Run(ctx context.Context, job Job) error {
	var payload casePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("payload of %s: %w", job.Kind, err)
	}

	cs, err := p.cases.Load(ctx, payload.CaseID)
	if err != nil {
		return err
	}
	// Проверка статуса обязательна: без неё отменённое обращение всё равно
	// уехало бы в GitHub.
	if cs == nil || cs.IssueNumber != 0 || cs.Status != statusPublishing {
		return nil
	}
	if cs.ProjectID == nil {
		return fmt.Errorf("case %s has no project", cs.ID)
	}

	project, err := LoadProject(ctx, p.cases.pool, *cs.ProjectID)
	if err != nil {
		return err
	}
	author, err := LoadUser(ctx, p.cases.pool, cs.UserID)
	if err != nil {
		return err
	}

	if !project.LabelsReady {
		if err := p.gh.EnsureLabels(ctx, project); err != nil {
			return err
		}
		if err := MarkLabelsReady(ctx, p.cases.pool, project.ID); err != nil {
			return err
		}
	}
	// Метка автора живёт вне базового набора: она появляется вместе с первым
	// обращением человека.
	if err := p.gh.createLabel(ctx, project, "author:"+author.Slug, authorLabelColor, "Автор обращения"); err != nil {
		return err
	}

	marker := caseMarker(cs.ID)
	number, url := 0, ""
	// На первой попытке дубля быть не может: создание issue повторов не делает,
	// и до второй попытки очереди тикета в GitHub нет. Лишний запрос стоил бы
	// секунды на каждом тикете.
	if job.Attempts > 1 {
		if number, url, err = p.gh.FindIssue(ctx, project, marker); err != nil {
			return err
		}
	}
	if number == 0 {
		labels := []string{"type:" + cs.Kind, "status:new", "author:" + author.Slug}
		if cs.Incomplete {
			labels = append(labels, "incomplete")
		}
		number, url, err = p.gh.CreateIssue(ctx, project, cs.Title, p.body(cs, author, marker), labels)
		if err != nil {
			return err
		}
	}

	published := false
	err = p.cases.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE cases SET status = 'published', issue_number = $2, issue_url = $3, updated_at = now()
			WHERE id = $1 AND status = 'publishing'`, cs.ID, number, url)
		if err != nil {
			return fmt.Errorf("publish case %s: %w", cs.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		published = true

		if err := addEvent(ctx, tx, cs.ID, "published", map[string]any{
			"issue": number, "incomplete": cs.Incomplete,
		}); err != nil {
			return err
		}
		return putNotify(ctx, tx, cs.ID, job.ID, publishedMessage(number, url, cs.Incomplete))
	})
	if err != nil {
		return err
	}
	if !published {
		return nil
	}

	p.log.Info("issue_created", "case_id", cs.ID, "project", project.Slug,
		"issue", number, "incomplete", cs.Incomplete)

	// Медиа не переживает обращение. Удаление стоит здесь, а не в конце
	// нормализации: до подтверждения саммари файл - последняя возможность
	// поймать неверно прочитанный скриншот.
	//
	// Сбой уборки не возвращает ошибку: тикет уже создан, и повтор работы вышел
	// бы на первой же проверке issue_number, ни разу не дойдя до файлов.
	// Подчищает почасовой сборщик, он смотрит и на опубликованные обращения.
	if err := p.cases.DropFiles(ctx, cs.ID); err != nil {
		p.log.Error("drop_files_failed", "case_id", cs.ID, "error", err)
	}
	return nil
}

// body собирает тело тикета: авторство, разделы контракта, пробелы и маркер.
//
// Авторство фиксируется телом, а не полем API: GitHub не даёт создать issue от
// чужого имени, автором станет владелец токена.
func (p *Publisher) body(cs *Case, author User, marker string) string {
	var b strings.Builder
	b.WriteString("Автор: " + authorName(author) + "\n\n")
	b.WriteString(cs.Summary)

	if len(cs.Gaps) > 0 {
		b.WriteString("\n\n## Не разобрано\n\n")
		for _, key := range cs.Gaps {
			if title := p.rules.Title(cs.Kind, key); title != "" {
				b.WriteString("- " + title + "\n")
			}
		}
		b.WriteString("\nАвтор этого не уточнил. Пробел назван явно: правдоподобная " +
			"выдумка была бы принята за факт.")
	}

	b.WriteString("\n\n" + marker)
	return b.String()
}

// caseMarker - скрытая метка обращения в теле тикета. По ней повтор работы
// узнаёт свой issue, если ответ на успешный запрос потерялся.
func caseMarker(caseID string) string { return "<!-- intake:case:" + caseID + " -->" }

func authorName(u User) string {
	name := strings.TrimSpace(u.First + " " + u.Last)
	if u.Username != "" {
		return name + " (@" + u.Username + ")"
	}
	return name
}

// publishFailedText отделяет отказ в правах от временного сбоя. Советовать
// «нажмите ещё раз» там, где токену не хватает прав, значит гонять автора по
// кругу: повтор не поможет, пока владелец не выдаст право заводить тикеты.
func publishFailedText(cause error) string {
	var apiErr *githubError
	if errors.As(cause, &apiErr) &&
		(apiErr.status == http.StatusForbidden || apiErr.status == http.StatusNotFound) {
		return "Тикет не создан: у сервиса нет прав заводить задачи в этом проекте. " +
			"Материал сохранён, я передал владельцу - опубликуем, как права появятся."
	}
	return "Не удалось создать тикет в GitHub. Нажмите «Публикую» ещё раз - материал на месте."
}

func publishedMessage(number int, url string, incomplete bool) string {
	text := fmt.Sprintf("Готово. Тикет #%d: %s", number, url)
	if incomplete {
		text += "\n\nЧасть вопросов осталась без ответа - тикет помечен как неполный, " +
			"пробелы перечислены в теле."
	}
	return text
}
