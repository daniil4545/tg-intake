package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	GitHubAPI     = "https://api.github.com"
	githubTimeout = 20 * time.Second
	// Просмотр ждёт человек, и ждёт он же за всех остальных авторов: бюджет
	// короткий, повторов нет, отказ сразу превращается в «статус недоступен».
	githubFastTimeout = 5 * time.Second
	githubRetries     = 3
	githubLimit       = 1 << 20
	// Сколько последних комментариев страниц просматриваем в поиске последнего.
	githubCommentPages = 5
	// Сколько последних issue просматриваем в поиске своего маркера. Дубль
	// ищется сразу после потерянного ответа, поэтому нужный тикет лежит в самом
	// начале списка.
	githubScan = 30
)

// GitHub - клиент Issues API на net/http, без SDK (раздел 1 architecture.md).
// Клиента два: рабочий с повторами для очереди и быстрый для хендлеров, где
// отказ GitHub морозил бы бота (раздел 6). Прокси нет: GitHub идёт напрямую.
type GitHub struct {
	token    string
	api      string
	http     *http.Client
	fast     *http.Client
	statuses Statuses
	log      *slog.Logger
}

// NewGitHub: api - базовый адрес, параметром ради тестов на httptest.Server.
func NewGitHub(token, api string, statuses Statuses, log *slog.Logger) *GitHub {
	return &GitHub{
		token:    token,
		api:      api,
		http:     &http.Client{Timeout: githubTimeout},
		fast:     &http.Client{Timeout: githubFastTimeout},
		statuses: statuses,
		log:      log,
	}
}

// Метки проекта. Статус тикета живёт меткой и остаётся единственным источником
// истины: сервис их только заводит и читает.
// Статусов здесь нет: они приходят из rules/statuses.json. Иначе добавленный в
// правила статус бот умел бы читать, но никогда не завёл бы в репозитории.
var baseLabels = []struct{ Name, Color, Desc string }{
	{"type:bug", "d73a4a", "Сервис ведёт себя не так, как ожидали"},
	{"type:feature", "a2eeef", "Нужно то, чего в сервисе нет"},
	{"type:question", "d876e3", "Нужен ответ, а не изменение в коде"},
	{"incomplete", "fbca04", "Контракт готовности недобран, пробелы в теле"},
}

const authorLabelColor = "ededed"

// PrepareProject заводит метки проекта и этим же проверяет право писать:
// метка создаётся записью, отказ в правах виден до того, как автор потратил
// интервью. Почему не чтение и почему каждый проект - раздел 6 architecture.md.
func (g *GitHub) PrepareProject(ctx context.Context, p Project) error {
	for _, l := range baseLabels {
		if err := g.createLabel(ctx, p, l.Name, l.Color, l.Desc); err != nil {
			return err
		}
	}
	for _, s := range g.statuses {
		if err := g.createLabel(ctx, p, s.Label, s.Color, "Статус: "+s.Title); err != nil {
			return err
		}
	}
	return nil
}

// EnsureLabels заводит недостающие метки проекта. Автосоздание метки при
// создании issue документацией не обещано, поэтому шаг явный.
func (g *GitHub) EnsureLabels(ctx context.Context, p Project) error {
	if err := g.PrepareProject(ctx, p); err != nil {
		return err
	}
	g.log.Info("labels_created", "project", p.Slug, "labels", len(baseLabels)+len(g.statuses))
	return nil
}

// CheckWrite проверяет право писать в Issues одной меткой и коротким бюджетом.
// Полный bootstrap здесь не годится: десять меток рабочим клиентом - это до
// двух минут с повторами, а зовут отсюда хендлер, который держит очередь
// апдейтов всех авторов. Остальные метки заведёт старт либо первая публикация.
func (g *GitHub) CheckWrite(ctx context.Context, p Project) error {
	l := baseLabels[0]
	body, err := json.Marshal(map[string]string{"name": l.Name, "color": l.Color, "description": l.Desc})
	if err != nil {
		return fmt.Errorf("build label %s: %w", l.Name, err)
	}

	_, _, err = g.send(ctx, g.fast, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/labels", p.Owner, p.Repo), body)
	var apiErr *githubError
	if errors.As(err, &apiErr) && apiErr.status == http.StatusUnprocessableEntity {
		return nil
	}
	return err
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
	raw, _, err := g.send(ctx, g.http, http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues", p.Owner, p.Repo), payload)
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
	issues, err := g.listIssues(ctx, p, githubScan, false)
	if err != nil {
		return 0, "", err
	}
	for _, issue := range issues {
		if strings.Contains(issue.Body, marker) {
			return issue.Number, issue.HTMLURL, nil
		}
	}
	return 0, "", nil
}

// Issue - тикет в том виде, в каком его читает сервис. Labels нужны просмотру,
// Body - поиску маркера, PullRequest - отсеву: REST GitHub считает issue каждый
// pull request, и в активном репозитории они вытесняют тикеты из окна.
type Issue struct {
	Number      int    `json:"number"`
	HTMLURL     string `json:"html_url"`
	Body        string `json:"body"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// LabelNames - имена меток тикета.
func (i Issue) LabelNames() []string {
	names := make([]string, 0, len(i.Labels))
	for _, l := range i.Labels {
		names = append(names, l.Name)
	}
	return names
}

// listIssues читает последние тикеты репозитория. Pull request'ы отсеиваются
// здесь, чтобы ни один вызывающий не забыл про них.
func (g *GitHub) listIssues(ctx context.Context, p Project, limit int, fast bool) ([]Issue, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues?state=all&per_page=%d&sort=created&direction=desc",
		p.Owner, p.Repo, limit)
	raw, err := g.get(ctx, path, fast)
	if err != nil {
		return nil, err
	}

	var issues []Issue
	if err := json.Unmarshal(raw, &issues); err != nil {
		return nil, fmt.Errorf("decode issue list: %w", err)
	}
	kept := issues[:0]
	for _, issue := range issues {
		if issue.PullRequest == nil {
			kept = append(kept, issue)
		}
	}
	return kept, nil
}

// ListIssues - список для просмотра: короткий бюджет, без повторов.
func (g *GitHub) ListIssues(ctx context.Context, p Project, limit int) ([]Issue, error) {
	return g.listIssues(ctx, p, limit, true)
}

// GetIssue читает один тикет. fast решает, чей это вызов: просмотр из хендлера
// или работа из очереди.
func (g *GitHub) GetIssue(ctx context.Context, p Project, number int, fast bool) (Issue, error) {
	raw, err := g.get(ctx, fmt.Sprintf("/repos/%s/%s/issues/%d", p.Owner, p.Repo, number), fast)
	if err != nil {
		return Issue{}, err
	}

	var issue Issue
	if err := json.Unmarshal(raw, &issue); err != nil {
		return Issue{}, fmt.Errorf("decode issue %d: %w", number, err)
	}
	return issue, nil
}

// LastComment - последний комментарий тикета. Комментарии приходят по
// возрастанию идентификатора, поэтому нужна последняя страница: листаем, пока
// страница полна, но не дальше пяти - пятьсот комментариев у внутреннего тикета
// означают, что что-то пошло не так, и это видно в логе.
func (g *GitHub) LastComment(ctx context.Context, p Project, number int) (string, error) {
	const perPage = 100
	// Бюджет на всю операцию: страниц может быть пять, а ждёт её человек, и
	// вместе с ним все остальные авторы.
	ctx, cancel := context.WithTimeout(ctx, githubFastTimeout)
	defer cancel()
	last := ""
	for page := 1; page <= githubCommentPages; page++ {
		path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=%d&page=%d",
			p.Owner, p.Repo, number, perPage, page)
		raw, err := g.get(ctx, path, true)
		if err != nil {
			return "", err
		}

		var comments []struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(raw, &comments); err != nil {
			return "", fmt.Errorf("decode comments of issue %d: %w", number, err)
		}
		if len(comments) > 0 {
			last = comments[len(comments)-1].Body
		}
		if len(comments) < perPage {
			return last, nil
		}
	}
	g.log.Warn("comments_truncated", "repo", p.Owner+"/"+p.Repo, "issue", number)
	return last, nil
}

// AddLabel добавляет метку, не трогая остальные: PUT затёр бы всё, включая
// проставленное владельцем вручную.
func (g *GitHub) AddLabel(ctx context.Context, p Project, number int, label string) error {
	body, err := json.Marshal(map[string][]string{"labels": {label}})
	if err != nil {
		return fmt.Errorf("build label request: %w", err)
	}
	_, err = g.call(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/issues/%d/labels", p.Owner, p.Repo, number), body)
	return err
}

// RemoveLabel снимает одну метку. 404 означает, что её и не было, - это не
// ошибка, а то состояние, которого мы добивались.
func (g *GitHub) RemoveLabel(ctx context.Context, p Project, number int, label string) error {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/labels/%s",
		p.Owner, p.Repo, number, url.PathEscape(label))
	_, err := g.call(ctx, http.MethodDelete, path, nil)
	var apiErr *githubError
	if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound {
		return nil
	}
	return err
}

// CloseIssue закрывает тикет как незапланированный: автор от него отказался, а
// не работа была сделана.
func (g *GitHub) CloseIssue(ctx context.Context, p Project, number int) error {
	body, err := json.Marshal(map[string]string{"state": "closed", "state_reason": "not_planned"})
	if err != nil {
		return fmt.Errorf("build close request: %w", err)
	}
	_, err = g.call(ctx, http.MethodPatch,
		fmt.Sprintf("/repos/%s/%s/issues/%d", p.Owner, p.Repo, number), body)
	return err
}

// githubError несёт код ответа: 422 на метке означает «уже есть», и отличить
// его от настоящего отказа можно только по статусу.
type githubError struct {
	status  int
	message string
}

func (e *githubError) Error() string { return fmt.Sprintf("github status %d: %s", e.status, e.message) }

// get - чтение с выбором бюджета. fast означает «зовут из хендлера»: одна
// попытка коротким клиентом, потому что автор ждёт ответа, а с ним и все
// остальные авторы.
func (g *GitHub) get(ctx context.Context, path string, fast bool) (json.RawMessage, error) {
	if fast {
		raw, _, err := g.send(ctx, g.fast, http.MethodGet, path, nil)
		return raw, err
	}
	return g.call(ctx, http.MethodGet, path, nil)
}

func (g *GitHub) call(ctx context.Context, method, path string, body []byte) (json.RawMessage, error) {
	for attempt := 0; ; attempt++ {
		raw, retry, err := g.send(ctx, g.http, method, path, body)
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
func (g *GitHub) send(ctx context.Context, client *http.Client, method, path string, body []byte) (json.RawMessage, bool, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.api+path, reader)
	if err != nil {
		return nil, false, fmt.Errorf("build github request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
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
		message := githubMessage(raw)
		// GitHub называет недостающее право заголовком. Без него «Resource not
		// accessible» не отличает «прав нет» от «токен не выдан на этот
		// репозиторий», и разбор упирается в догадки.
		if need := resp.Header.Get("X-Accepted-GitHub-Permissions"); need != "" {
			message += " (нужно: " + need + ")"
		}
		return nil, retry, &githubError{status: resp.StatusCode, message: message}
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
	items, err := p.cases.Items(ctx, cs.ID)
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
		labels := []string{"type:" + cs.Kind, labelNew, "author:" + author.Slug}
		if cs.Incomplete {
			labels = append(labels, "incomplete")
		}
		body := p.body(cs, author, collectLinks(items), marker)
		number, url, err = p.gh.CreateIssue(ctx, project, cs.Title, body, labels)
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
		// Панель возвращается в исходное состояние тем же сообщением: обращение
		// доиграно, «Готово» больше не по чему нажимать.
		return putNotifyKey(ctx, tx, cs.ID, strconv.FormatInt(job.ID, 10),
			publishedMessage(number, url, cs.Incomplete), keysHome)
	})
	if err != nil {
		return err
	}
	if !published {
		return nil
	}

	p.log.Info("issue_created", "case_id", cs.ID, "project", project.Slug,
		"issue", number, "incomplete", cs.Incomplete)

	// Медиа не переживает обращение; удаление здесь, а не в нормализации: до
	// подтверждения саммари файл ловит неверно прочитанный скриншот. Сбой не
	// ошибка: повтор вышел бы на issue_number, подчистит почасовой сборщик.
	if err := p.cases.DropFiles(ctx, cs.ID); err != nil {
		p.log.Error("drop_files_failed", "case_id", cs.ID, "error", err)
	}
	return nil
}

// body собирает тело тикета: авторство, разделы контракта, пробелы и маркер.
//
// Авторство фиксируется телом, а не полем API: GitHub не даёт создать issue от
// чужого имени, автором станет владелец токена.
func (p *Publisher) body(cs *Case, author User, links []string, marker string) string {
	var b strings.Builder
	b.WriteString("Автор: " + authorName(author) + "\n\n")
	b.WriteString(cs.Summary)

	// Адреса из сырья идут отдельным разделом и целиком: тот, кто возьмёт тикет,
	// открывает карточку сам, а пересказ модели ведёт в никуда.
	if len(links) > 0 {
		b.WriteString("\n\n## Ссылки\n\n")
		for _, link := range links {
			b.WriteString("- " + link + "\n")
		}
		b.WriteString("\nПрислано автором вместе с материалом обращения.")
	}

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
			"Материал сохранён. Напишите владельцу сервиса - когда право появится, " +
			"нажмите «Публикую» ещё раз."
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

// Repo - репозиторий в том виде, в каком его читает заведение проекта.
type Repo struct {
	Name        string `json:"name"`
	FullName    string `json:"full_name"`
	Description string `json:"description"`
}

// GetRepo читает репозиторий быстрым клиентом: зовут из хендлера, автор ждёт.
// 404 означает «нет репозитория либо токен его не видит»; для fine-grained PAT
// это одно и то же, и различать их сервису незачем.
func (g *GitHub) GetRepo(ctx context.Context, owner, repo string) (Repo, error) {
	raw, err := g.get(ctx, fmt.Sprintf("/repos/%s/%s", owner, repo), true)
	if err != nil {
		return Repo{}, err
	}

	var out Repo
	if err := json.Unmarshal(raw, &out); err != nil {
		return Repo{}, fmt.Errorf("decode repo %s/%s: %w", owner, repo, err)
	}
	return out, nil
}

// GetReadme отдаёт текст README. Пустая строка означает, что его нет: контекст
// проекта тогда соберётся из описания репозитория.
//
// Содержимое приходит в base64 внутри JSON - сырой текст потребовал бы своего
// заголовка Accept, а клиент шлёт один на все запросы.
func (g *GitHub) GetReadme(ctx context.Context, owner, repo string) (string, error) {
	raw, err := g.get(ctx, fmt.Sprintf("/repos/%s/%s/readme", owner, repo), true)
	if err != nil {
		if isNotFound(err) {
			return "", nil
		}
		return "", err
	}

	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode readme of %s/%s: %w", owner, repo, err)
	}
	if out.Encoding != "base64" {
		return out.Content, nil
	}

	// Переносы строк внутри base64 - обычное дело для этого ответа, декодер их
	// не принимает.
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return "", fmt.Errorf("decode readme body of %s/%s: %w", owner, repo, err)
	}
	return string(decoded), nil
}
