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
	// Окно тикетов: пересечение почти всегда со свежим тикетом, а полный список
	// активного репозитория не влез бы в контекст и стоил бы страниц запросов.
	overlapIssues = 50
	// Бюджеты выдержек. Один docs/architecture.md весит под 50 КБ: без обреза он
	// один занял бы всё окно, вытеснив CHANGELOG, ради которого сверка и заводится.
	overlapDocChars = 20000
	overlapAllChars = 60000
	// Предел списка: подсказка автору, а не отчёт. Лишние пункты размывают сильное
	// совпадение, ради которого он и читается.
	maxOverlapItems = 4
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
// не отдаёт: любой сбой - недоступный GitHub, вышедший бюджет, мусор от модели -
// стоит подсказки, а не обращения. Обратное поведение отняло бы у бизнеса
// единственный вход в разработку ради необязательной проверки.
func (o *Overlap) Check(ctx context.Context, cs *Case, p Project, title, brief, body string) string {
	start := time.Now()

	repo, err := o.gh.GetRepo(ctx, p.Owner, p.Repo)
	if err != nil {
		o.log.Warn("overlap_skipped", "case_id", cs.ID, "step", "repo", "error", err)
		return ""
	}
	docs, err := o.gh.TreeDocs(ctx, p, repo.DefaultBranch)
	if err != nil {
		o.log.Warn("overlap_skipped", "case_id", cs.ID, "step", "tree", "error", err)
		return ""
	}
	issues, err := o.gh.ListIssues(ctx, p, overlapIssues)
	if err != nil {
		o.log.Warn("overlap_skipped", "case_id", cs.ID, "step", "issues", "error", err)
		return ""
	}

	loaded, chars := o.load(ctx, cs, p, repo.DefaultBranch, docs)
	if len(issues) == 0 && len(loaded) == 0 {
		o.log.Info("overlap_empty_sources", "case_id", cs.ID, "project", p.Slug)
		return ""
	}

	var out overlapOut
	messages := lookupMessages(overlapPrefix, p.Context,
		issuesMessage(issues)+"\n\n"+docsMessage(loaded),
		[]Message{{Role: "user", Parts: []Part{TextPart(draftMessage(title, brief, body))}}})
	if err := o.complete(ctx, messages, &out); err != nil {
		o.log.Warn("overlap_skipped", "case_id", cs.ID, "step", "model", "error", err)
		return ""
	}

	items, dropped := keepFound(out.Items, issues, loaded)
	if dropped > 0 {
		// Номер и путь, которых модель не видела, - выдумка: автор принял бы её за
		// факт и не завёл нужный тикет.
		o.log.Warn("overlap_invented", "case_id", cs.ID, "dropped", dropped)
	}
	o.log.Info("overlap_done", "case_id", cs.ID, "project", p.Slug,
		"issues", len(issues), "docs", len(loaded), "chars", chars,
		"found", len(items), "ms", time.Since(start).Milliseconds())

	return overlapList(items, issues, p, repo.DefaultBranch)
}

// load читает документы состояния проекта, которые есть в дереве. Отсутствие
// файла - обычное дело: комплект документов у проектов разный.
func (o *Overlap) load(ctx context.Context, cs *Case, p Project, ref string, docs []DocFile) ([]docText, int) {
	known := make(map[string]bool, len(docs))
	for _, d := range docs {
		known[d.Path] = true
	}

	loaded := make([]docText, 0, len(overlapDocs))
	chars := 0
	for _, path := range overlapDocs {
		left := overlapAllChars - chars
		if left <= 0 {
			break
		}
		if !known[path] {
			continue
		}
		text, err := o.gh.File(ctx, p, path, ref)
		if err != nil {
			o.log.Warn("overlap_file_failed", "case_id", cs.ID, "path", path, "error", err)
			continue
		}
		text = cutDoc(text, min(overlapDocChars, left))
		chars += utf8.RuneCountInString(text)
		loaded = append(loaded, docText{Path: path, Text: text})
	}
	return loaded, chars
}

func (o *Overlap) complete(ctx context.Context, messages []Message, out *overlapOut) error {
	raw, err := o.llm.Complete(ctx, Request{
		Step:       stepOverlap,
		Model:      o.model.Name,
		Reasoning:  o.model.Reasoning,
		MaxTokens:  llmMaxTokens,
		Messages:   messages,
		SchemaName: stepOverlap,
		Schema:     overlapSchema,
	})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", stepOverlap, err)
	}
	return nil
}

// keepFound оставляет пункты, опирающиеся на прочитанное: номер обязан быть среди
// выданных тикетов, путь - среди прочитанных файлов. Второе значение - сколько
// отброшено. Флаг found от модели не смотрим: список строят пункты, а «нашёл» без
// пункта не значит ничего.
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
		item.Note = scrubContacts(strings.TrimSpace(item.Note))
		switch {
		case item.Note == "":
			dropped++
		case item.Issue != 0 && known[item.Issue]:
			kept = append(kept, overlapItem{Issue: item.Issue, Note: item.Note})
		case item.Issue == 0 && read[item.Path]:
			kept = append(kept, overlapItem{Path: item.Path, Note: item.Note})
		default:
			dropped++
		}
		if len(kept) == maxOverlapItems {
			break
		}
	}
	return kept, dropped
}

// overlapList - готовый список пунктов. Ссылку строит Go по номеру и пути: модель
// адресов не пишет - тот же довод, что у ответа по документации.
func overlapList(items []overlapItem, issues []Issue, p Project, ref string) string {
	titles := make(map[int]Issue, len(issues))
	for _, issue := range issues {
		titles[issue.Number] = issue
	}

	var b strings.Builder
	for _, item := range items {
		if item.Issue != 0 {
			issue := titles[item.Issue]
			fmt.Fprintf(&b, "- [Тикет #%d %s](%s)%s: %s\n",
				item.Issue, issue.Title, issue.HTMLURL, closedMark(issue), item.Note)
			continue
		}
		links := sourceLinks(p, ref, []string{item.Path})
		fmt.Fprintf(&b, "- [%s](%s): %s\n", item.Path, links[0], item.Note)
	}
	return strings.TrimRight(b.String(), "\n")
}

// closedMark отмечает закрытый тикет: для автора это разница между «уже делают» и
// «сделано или отклонено».
func closedMark(issue Issue) string {
	if issue.State == "closed" {
		return ", закрыт"
	}
	return ""
}

// issuesMessage - волатильная часть: тикеты репозитория. Тело не идёт вовсе -
// пятьдесят описаний не влезут в окно, а для совпадения по сути хватает
// заголовка, состояния и меток.
func issuesMessage(issues []Issue) string {
	if len(issues) == 0 {
		return "Тикетов в репозитории нет."
	}

	var b strings.Builder
	b.WriteString("Последние тикеты репозитория (номер, состояние, метки, заголовок):\n")
	for _, issue := range issues {
		fmt.Fprintf(&b, "#%d [%s] %s %s\n", issue.Number, issue.State,
			strings.Join(issue.LabelNames(), ","), issue.Title)
	}
	return strings.TrimRight(b.String(), "\n")
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
