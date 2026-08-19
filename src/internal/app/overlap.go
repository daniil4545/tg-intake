package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

const stepOverlap = "overlap"

const (
	// Свой бюджет: сверка идёт последней в работе саммари, и без потолка её
	// повторы съели бы время, оставшееся транзакции. Готовое саммари ушло бы в
	// повтор вместе со сверкой - ровно то, что шаг обязан не делать.
	overlapBudget = 60 * time.Second
	// Окно ленты issues. Считается записями, а не тикетами: pull request'ы
	// приходят тем же списком и отсеиваются после, поэтому окно взято с запасом.
	overlapIssues = 100
	// Бюджеты выдержек. Один docs/architecture.md весит под 50 КБ: без обреза он
	// один занял бы всё окно, вытеснив CHANGELOG, ради которого сверка и заводится.
	overlapDocChars = 20000
	overlapAllChars = 60000
	// Пределы вывода модели. Список - подсказка, а не отчёт; длинная заметка
	// вытолкнула бы кнопки саммари в отдельное сообщение, а многострочная
	// сломала бы пункт списка в теле issue.
	maxOverlapItems   = 4
	overlapNoteChars  = 200
	overlapTitleChars = 100
)

// Документы состояния проекта в порядке убывания пользы. Отбора моделью здесь
// нет намеренно: это второй вызов и второй источник выдумки ради гибкости,
// которой у сверки нет - состояние проекта живёт в этих четырёх файлах.
var overlapDocs = []string{
	"CHANGELOG.md",
	"README.md",
	"docs/backlog.md",
	"docs/architecture.md",
}

// Метки, которые сверке о чём-то говорят. Метка автора не проходит: ФИО
// сотрудников незачем отправлять в модель ради поиска похожего тикета.
var overlapLabels = []string{"type:", "status:", "incomplete"}

var overlapPrefix = strings.ReplaceAll(mustPrompt("overlap.md"), "{{DOCMAP}}", docMap)

var overlapSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"found": {"type": "boolean"},
		"items": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"issue": {"type": "integer"},
					"path": {"type": "string"},
					"note": {"type": "string"}
				},
				"required": ["issue", "path", "note"],
				"additionalProperties": false
			}
		}
	},
	"required": ["found", "items"],
	"additionalProperties": false
}`)

// overlapItem - одно найденное пересечение: либо тикет, либо документ. Note
// говорит, что именно совпало: без факта пункт бесполезен автору.
type overlapItem struct {
	Issue int    `json:"issue"`
	Path  string `json:"path"`
	Note  string `json:"note"`
}

type overlapOut struct {
	Found bool          `json:"found"`
	Items []overlapItem `json:"items"`
}

// Overlap - сверка готового черновика с состоянием проекта: тикеты репозитория и
// документы, где живёт «что уже сделано». Шаг необязательный по устройству: он
// подсказывает автору, но не имеет права остановить обращение.
type Overlap struct {
	gh    *GitHub
	llm   *OpenRouter
	log   *slog.Logger
	model DialogModel
}

func NewOverlap(gh *GitHub, llm *OpenRouter, log *slog.Logger, model DialogModel) *Overlap {
	return &Overlap{gh: gh, llm: llm, log: log, model: model}
}

// Check возвращает markdown-список пересечений или пустую строку. Ошибку наружу
// не отдаёт: любой сбой стоит подсказки, а не обращения. Обратное поведение
// отняло бы у бизнеса единственный вход в разработку ради необязательной проверки.
func (o *Overlap) Check(ctx context.Context, caseID string, p Project, title, brief, body string) string {
	// Бюджета работы может уже не остаться: саммари перед этим тратило свой на
	// модель с повторами. Начатая на остатке сверка отняла бы время у транзакции.
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < overlapBudget {
		o.log.Warn("overlap_skipped", "case_id", caseID, "step", "budget")
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, overlapBudget)
	defer cancel()

	start := time.Now()
	repo, err := o.gh.GetRepo(ctx, p.Owner, p.Repo)
	if err != nil {
		o.log.Warn("overlap_skipped", "case_id", caseID, "step", "repo", "error", err)
		return ""
	}
	issues, err := o.gh.listIssues(ctx, p, overlapIssues, sortCreated, false)
	if err != nil {
		o.log.Warn("overlap_skipped", "case_id", caseID, "step", "issues", "error", err)
		return ""
	}

	loaded, chars := o.load(ctx, caseID, p, repo.DefaultBranch)
	if len(issues) == 0 && len(loaded) == 0 {
		o.log.Info("overlap_empty_sources", "case_id", caseID, "project", p.Slug)
		return ""
	}

	// Документы впереди тикетов: они меняются на релизе, а лента тикетов - от
	// каждой публикации, и общий префикс запроса иначе обесценивался бы чаще.
	volatile := docsMessage(loaded, "Выдержки из документов проекта:") +
		"\n\n" + issuesMessage(issues)
	draft := []Message{{Role: "user", Parts: []Part{TextPart(draftMessage(title, brief, body))}}}

	var out overlapOut
	messages := lookupMessages(overlapPrefix, p.Context, volatile, draft)
	if err := complete(ctx, o.llm, o.model, stepOverlap, overlapSchema, messages, &out); err != nil {
		o.log.Warn("overlap_skipped", "case_id", caseID, "step", "model", "error", err)
		return ""
	}

	items, dropped := keepFound(out.Items, issues, loaded)
	if dropped > 0 {
		// Номер и путь, которых модель не видела, - выдумка: автор принял бы её за
		// факт и не завёл нужный тикет.
		o.log.Warn("overlap_invented", "case_id", caseID, "dropped", dropped)
	}
	o.log.Info("overlap_done", "case_id", caseID, "project", p.Slug,
		"issues", len(issues), "docs", len(loaded), "chars", chars,
		"found", len(items), "ms", time.Since(start).Milliseconds())

	return overlapList(items, issues, p, repo.DefaultBranch)
}

// load читает документы состояния проекта. Дерево репозитория для этого не
// нужно: отсутствующий файл отвечает 404, а комплект документов у проектов
// разный, и это обычное дело, а не отказ.
func (o *Overlap) load(ctx context.Context, caseID string, p Project, ref string) ([]docText, int) {
	loaded := make([]docText, 0, len(overlapDocs))
	chars := 0
	for _, path := range overlapDocs {
		left := overlapAllChars - chars
		if left <= 0 {
			break
		}
		text, err := o.gh.File(ctx, p, path, ref)
		if err != nil {
			if !isDenied(err) {
				o.log.Warn("overlap_file_failed", "case_id", caseID, "path", path, "error", err)
			}
			continue
		}
		text = cutDoc(text, min(overlapDocChars, left))
		chars += utf8.RuneCountInString(text)
		loaded = append(loaded, docText{Path: path, Text: text})
	}
	return loaded, chars
}

// keepFound оставляет пункты, опирающиеся на прочитанное: номер обязан быть среди
// выданных модели тикетов, путь - среди прочитанных файлов. Второе значение -
// сколько отброшено. Флаг found не смотрится вовсе: список строят пункты.
func keepFound(items []overlapItem, issues []Issue, loaded []docText) ([]overlapItem, int) {
	known := make(map[int]bool, len(issues))
	for _, issue := range issues {
		known[issue.Number] = true
	}
	read := make(map[string]bool, len(loaded))
	for _, d := range loaded {
		read[d.Path] = true
	}

	var kept []overlapItem
	dropped := 0
	for _, item := range items {
		item.Path = strings.TrimSpace(item.Path)
		item.Note = cutRunes(scrubContacts(oneLine(item.Note)), overlapNoteChars)
		// Пункт называет либо тикет, либо документ: заполненное разом не говорит,
		// чем именно совпало, и ссылка вышла бы наугад. Второй пункт про тот же
		// источник тоже не проходит - в списке он стал бы повтором строки.
		var found overlapItem
		switch {
		case item.Note == "":
		case item.Issue != 0 && item.Path == "" && known[item.Issue]:
			found = overlapItem{Issue: item.Issue, Note: item.Note}
			known[item.Issue] = false
		case item.Issue == 0 && read[item.Path]:
			found = overlapItem{Path: item.Path, Note: item.Note}
			read[item.Path] = false
		}
		if found.Issue == 0 && found.Path == "" {
			dropped++
			continue
		}
		if len(kept) < maxOverlapItems {
			kept = append(kept, found)
		}
	}
	return kept, dropped
}

// overlapList - готовый список пунктов. Ссылку берёт Go: у документа собирает по
// ветке из ответа GitHub, у тикета берёт адрес из того же ответа - модель адресов
// не пишет, тот же довод, что у ответа по документации.
func overlapList(items []overlapItem, issues []Issue, p Project, ref string) string {
	found := make(map[int]Issue, len(issues))
	for _, issue := range issues {
		found[issue.Number] = issue
	}

	var b strings.Builder
	for _, item := range items {
		if item.Issue != 0 {
			issue := found[item.Issue]
			state := ""
			if issue.State == "closed" {
				state = ", закрыт"
			}
			fmt.Fprintf(&b, "- [Тикет #%d %s](%s)%s: %s\n",
				item.Issue, linkText(issue.Title), issue.HTMLURL, state, item.Note)
			continue
		}
		links := sourceLinks(p, ref, []string{item.Path})
		fmt.Fprintf(&b, "- [%s](%s): %s\n", item.Path, links[0], item.Note)
	}
	return strings.TrimRight(b.String(), "\n")
}

// linkText готовит чужой текст к подстановке в markdown-ссылку. Заголовок тикета
// пишет посторонний человек: скобки внутри него увели бы ссылку мимо собранной
// Go, а адрес в тексте увёл бы туда же самого автора.
func linkText(text string) string {
	text = linkRe.ReplaceAllString(oneLine(text), "[ссылка]")
	text = strings.NewReplacer("[", "", "]", "", "(", "", ")", "").Replace(text)
	return cutRunes(text, overlapTitleChars)
}

// issuesMessage - волатильная часть: тикеты репозитория. Тело не идёт вовсе -
// сотня описаний не влезет в окно, а для совпадения по сути хватает заголовка,
// состояния и меток.
func issuesMessage(issues []Issue) string {
	if len(issues) == 0 {
		return "Тикетов в репозитории нет."
	}

	var b strings.Builder
	b.WriteString("Последние тикеты репозитория (номер, состояние, метки, заголовок). " +
		"Список - окно последних, старые тикеты в него не попали:\n")
	for _, issue := range issues {
		fmt.Fprintf(&b, "#%d [%s] %s %s\n", issue.Number, issue.State,
			strings.Join(keepLabels(issue.LabelNames()), ","), linkText(issue.Title))
	}
	return strings.TrimRight(b.String(), "\n")
}

// keepLabels отсеивает метки, которые сверке ничего не говорят. Метка автора
// несёт ФИО сотрудника, и в модель она не уходит.
func keepLabels(labels []string) []string {
	var kept []string
	for _, label := range labels {
		for _, prefix := range overlapLabels {
			if strings.HasPrefix(label, prefix) {
				kept = append(kept, label)
				break
			}
		}
	}
	return kept
}

// draftMessage - черновик тикета последним сообщением: он меняется от обращения к
// обращению чаще всего и потому стоит в самом хвосте запроса.
func draftMessage(title, brief, body string) string {
	var b strings.Builder
	b.WriteString("Черновик тикета:\n\n" + title + "\n\n")
	if brief != "" {
		b.WriteString(brief + "\n\n")
	}
	b.WriteString(body)
	return b.String()
}
