package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// R9 (docs/specs/ticket-form.md §10, AGENTS.md принцип 6): всё, что хоть
// одним путём попадает во ввод модели или в тело issue - метки GitHub,
// словари сверки, Unclear, ошибки и логи, метки протокола и источника,
// маски. Байт в байт: стиль не меняется. Чат - texts.go.

// msgLookupNoAnswer - ответ "не нашёл": ложится в answer_ready и оттуда
// возвращается моделью через Cases.history на следующем ходу, поэтому живёт
// здесь, а не в texts.go, хотя это и реплика чата.
const msgLookupNoAnswer = "В документации проекта ответа на это нет. Если нужна правка или " +
	"что-то не работает - нажмите «Создать тикет»."

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

const modelStatusLabelPrefix = "Статус: "

const modelErrDownloadFailed = "файл не скачался"

const modelErrFileUnread = "файл не прочитан"

const modelErrFileUnreadScreenshot = "файл не прочитан"

const modelErrSpeechUnrecognized = "речь не распознана"

const modelErrScreenshotSchema = "разбор скриншота не по схеме"

const modelRawProtocolPrefix = "Протокол сырья:\n\n"

const modelExcerptsHeading = "Выдержки из документов проекта:"

const modelGithubPermissionNeeded = " (нужно: "

// lookupMessages собирает сообщения запроса. Порядок обязателен: стабильный
// префикс первым сообщением, волатильное вторым, история разговора последней.
// Любая изменяющаяся строка перед промтом молча гасит кэш провайдера.
func lookupMessages(prefix, projectContext, volatile string, history []Message) []Message {
	messages := []Message{
		{Role: "system", Parts: []Part{TextPart(prefix + "\n\n## Проект\n\n" + projectContext)}},
		{Role: "user", Parts: []Part{TextPart(volatile)}},
	}
	return append(messages, history...)
}

// formatExtract превращает разбор в строку протокола: дальше с ним работает
// текстовая модель, и структура ей нужна как текст, а не как JSON.
func formatExtract(e screenshotExtract) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(e.Screen))
	for _, f := range e.Facts {
		fmt.Fprintf(&b, "\n   %s: %s", strings.TrimSpace(f.Label), strings.TrimSpace(f.Value))
	}
	if relevant := strings.TrimSpace(e.Relevant); relevant != "" {
		b.WriteString("\n   связь с обращением: " + relevant)
	}
	if len(*e.Unreadable) > 0 {
		b.WriteString("\n   не прочитано: " + strings.Join(*e.Unreadable, "; "))
	}
	return strings.TrimSpace(b.String())
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

// reservedHeadings - разделы тела, которые пишет Go: второй такой же заголовок
// от модели сделал бы тело тикета неоднозначным.
var reservedHeadings = []string{"Кратко", "Ссылки", "Пересечения"}

var (
	stubTails = []string{"не указано", "не указан", "не указана", "не указаны", "неизвестно",
		"не известно", "не разобрано", "неясно", "не ясно", "не сообщил", "не сообщила"}
	stubPhrases = []string{"нет данных", "данных нет", "нет информации", "информации нет",
		"информация отсутствует", "данные отсутствуют", "не удалось определить",
		"уточнить не удалось", "не сообщается"}
)

func scrubContacts(text string) string {
	text = emailRe.ReplaceAllString(text, "[почта]")
	text = cardRe.ReplaceAllString(text, "[карта]")
	return phoneRe.ReplaceAllString(text, "[телефон]")
}

// titleStopWords - служебные слова, с которых заголовок начинать нельзя: тип
// тикета виден по метке, а в списке видно только заголовок.
var titleStopWords = []string{"проблема", "баг", "ошибка", "просьба", "вопрос", "запрос"}

// renderSections собирает тело саммари в markdown - тот же текст уходит и в
// issue, и автору. Разделы идут в порядке модели под её заголовками: форма
// тикета следует материалу. Закрытый пункт ядра, который модель не покрыла
// разделом, дописывается из собранного интервью, а нет ни разделов, ни ядра -
// тело собирается из протокола сырья: ни одна идея не выбрасывается, и держит
// это Go, а не промт. Раздел по незакрытому пункту остаётся: это слова автора,
// а строка «Не уточнено» всё равно называет пункт пробелом.
func (i *Interview) renderSections(cs *Case, sections []Section) string {
	var b strings.Builder
	covered := make(map[string]bool, len(sections))
	for _, s := range sections {
		covered[s.Key] = true
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", scrubContacts(strings.TrimSpace(s.Heading)),
			scrubContacts(strings.TrimSpace(s.Text)))
	}
	for _, item := range i.rules.Items(cs.Kind) {
		text := strings.TrimSpace(cs.Filled[item.Key])
		if covered[item.Key] || text == "" {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", item.Title, scrubContacts(text))
	}
	if b.Len() == 0 && strings.TrimSpace(cs.Protocol) != "" {
		fmt.Fprintf(&b, "## Материал обращения\n\n%s", scrubContacts(strings.TrimSpace(cs.Protocol)))
	}
	return strings.TrimSpace(b.String())
}

// dialogMessages - стабильный префикс и волатильный протокол сырья, первые
// два сообщения хода интервью (вызывается из Interview.dialog).
func dialogMessages(prefix, projectContext, protocol string) []Message {
	return []Message{
		{Role: "system", Parts: []Part{TextPart(prefix + "\n\n## Проект\n\n" + projectContext)}},
		{Role: "user", Parts: []Part{TextPart("Протокол сырья:\n\n" + protocol)}},
	}
}

func questionList(questions []Question) string {
	var b strings.Builder
	for n, q := range questions {
		fmt.Fprintf(&b, "%d. %s\n", n+1, q.Text)
		if suggested := strings.TrimSpace(q.Suggested); suggested != "" {
			fmt.Fprintf(&b, "   Предполагаю: %s\n", suggested)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// history восстанавливает разговор из журнала. Отдельной таблицы у него нет:
// диалог по природе append-only, а case_events уже пишется в тех же
// транзакциях, что и смена статуса. Показанное саммари - такая же реплика бота,
// как вопрос раунда: автор правит именно его. Ответ по документации идёт сюда
// же - разговор, пришедший из режима вопроса, уже установил факты, и
// переспрашивать их интервью не должно. Вопроса автора здесь нет: его слова
// целиком лежат в протоколе сырья, который подаётся отдельным сообщением.
func (c *Cases) history(ctx context.Context, caseID string) ([]Message, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT kind, payload FROM case_events
		WHERE case_id = $1
		  AND kind IN ('round_asked', 'answer_given', 'summary_ready', 'answer_ready')
		ORDER BY id`, caseID)
	if err != nil {
		return nil, fmt.Errorf("query history of case %s: %w", caseID, err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var kind string
		var payload []byte
		if err := rows.Scan(&kind, &payload); err != nil {
			return nil, fmt.Errorf("scan history event: %w", err)
		}

		switch kind {
		case "round_asked":
			var p struct {
				Questions []Question `json:"questions"`
			}
			if err := json.Unmarshal(payload, &p); err != nil {
				return nil, fmt.Errorf("decode asked round: %w", err)
			}
			messages = append(messages, Message{
				Role:  "assistant",
				Parts: []Part{TextPart(questionList(p.Questions))},
			})
		case "answer_given", "answer_ready":
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(payload, &p); err != nil {
				return nil, fmt.Errorf("decode %s: %w", kind, err)
			}
			if p.Text == "" {
				continue
			}
			role := "user"
			if kind == "answer_ready" {
				role = "assistant"
			}
			messages = append(messages, Message{Role: role, Parts: []Part{TextPart(p.Text)}})
		case "summary_ready":
			var p struct {
				Title   string `json:"title"`
				Body    string `json:"body"`
				Overlap string `json:"overlap"`
			}
			if err := json.Unmarshal(payload, &p); err != nil {
				return nil, fmt.Errorf("decode shown summary: %w", err)
			}
			// Обращение начато до выката: снимка в событии нет, и подставить
			// вместо него нечего.
			if p.Body == "" {
				continue
			}
			shown := p.Title + "\n\n" + p.Body
			// Пересечения показаны автору той же репликой, и следующий его ответ
			// часто отвечает именно им: без них ход переспросит мимо.
			if p.Overlap != "" {
				shown += "\n\nПохоже, часть этого уже есть:\n\n" + p.Overlap
			}
			messages = append(messages, Message{
				Role:  "assistant",
				Parts: []Part{TextPart(shown)},
			})
		}
	}
	return messages, rows.Err()
}

// errSlugTaken - занятый ключ проекта: репозиторий уже привязан к другому slug.
func errSlugTaken(slug, owner, repo string) error {
	return fmt.Errorf("%w: %s занят репозиторием %s/%s", ErrSlugTaken, slug, owner, repo)
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

const modelSelectedFilesHeading = "Содержимое отобранных файлов:"

const modelDocCutMark = "\n\n[файл обрезан]"

func withLinks(body string, links []string) string {
	if len(links) == 0 {
		return body
	}
	label := "Источник: "
	if len(links) > 1 {
		label = "Источники:\n"
	}
	return body + "\n\n" + label + strings.Join(links, "\n")
}

// treeMessage - волатильная часть хода отбора: что за файлы есть в репозитории.
func treeMessage(docs []DocFile) string {
	var b strings.Builder
	b.WriteString("Файлы документации в репозитории (путь и размер в байтах):\n")
	for n, d := range docs {
		if n == maxTreePaths {
			// Без пометки модель считает список полным, и «не нашёл» становится
			// неверным: файл с ответом мог остаться за обрезом.
			b.WriteString("[список обрезан]\n")
			break
		}
		fmt.Fprintf(&b, "%s (%d)\n", d.Path, d.Size)
	}
	return strings.TrimRight(b.String(), "\n")
}

// docsMessage - волатильная часть: содержимое прочитанных файлов. Заголовок
// параметром: у отбора это «отобранные файлы», у сверки - выдержки из четырёх
// известных документов, которые никто не выбирал.
func docsMessage(loaded []docText, header string) string {
	if len(loaded) == 0 {
		return "Прочитать не удалось ни одного файла документации."
	}

	var b strings.Builder
	b.WriteString(header + "\n")
	for _, d := range loaded {
		fmt.Fprintf(&b, "\n### %s\n\n%s\n", d.Path, d.Text)
	}
	return b.String()
}

var itemLabel = map[string]string{
	"text":  "текст",
	"link":  "ссылка",
	"voice": "голосовое",
	"photo": "скриншот",
}

func itemLine(it Item) string {
	label := itemLabel[it.Kind]
	if label == "" {
		label = it.Kind
	}
	if it.Forwarded {
		label += ", переслано (не слова автора)"
	}

	if it.Status == "failed" {
		reason := strings.TrimSpace(it.Error)
		if reason == "" {
			reason = "причина неизвестна"
		}
		// Провал виден строкой, а не пропуском: модель должна видеть пробел, а
		// не достраивать его сама.
		return label + ": не удалось разобрать: " + oneLine(reason)
	}

	body := strings.TrimSpace(it.Normalized)
	caption := strings.TrimSpace(it.SourceText)
	if body == "" {
		body = caption
		caption = ""
	}
	if body == "" {
		return ""
	}
	// Подпись под пересланным медиа автор набирает сам, поэтому она идёт
	// отдельной строкой и как его слова: пометка «не слова автора» относится к
	// содержимому элемента, а не к тому, что автор написал под ним.
	if caption != "" {
		return label + ": " + body + "\n   слова автора: " + caption
	}
	return label + ": " + body
}

const modelAuthorLabelDesc = "Автор обращения"

// body собирает тело тикета: авторство, разделы саммари, незакрытое ядро и маркер.
//
// Авторство фиксируется телом, а не полем API: GitHub не даёт создать issue от
// чужого имени, автором станет владелец токена.
func (p *Publisher) body(cs *Case, author User, links []string, marker string) string {
	var b strings.Builder
	b.WriteString("Автор: " + authorName(author) + "\n\n")
	// Кратко идёт первым разделом: тот, кто возьмёт тикет, читает суть до
	// разделов контракта. Пусто оно только у тикетов, заведённых до появления
	// поля.
	if cs.Brief != "" {
		b.WriteString("## Кратко\n\n" + cs.Brief + "\n\n")
	}
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

	// Пересечения идут после ссылок и до пробелов: это не часть обращения, а
	// найденный сервисом контекст, и берущему тикет он нужен раньше, чем список
	// того, чего автор не уточнил.
	if cs.Overlap != "" {
		b.WriteString("\n\n## Пересечения\n\n" + cs.Overlap + "\n")
		b.WriteString("\nНашёл бот при сборке тикета и показал автору: он видел этот " +
			"список и всё равно завёл тикет.")
	}

	// Незакрытое ядро - одной строкой в конце: пробел назван явно, правдоподобная
	// выдумка была бы принята за факт. Считается так же, как метка incomplete.
	if unclear := p.rules.Unclear(cs.Kind, cs.Filled); unclear != "" {
		b.WriteString("\n\n---\n" + unclear)
	}

	b.WriteString("\n\n" + marker)
	return b.String()
}

// projectSource* - откуда взялось название и контекст проекта: своими словами,
// из полей репозитория или собрано моделью. Значение уходит в projectSourceNote.
const (
	projectSourceAuthor = "автор"
	projectSourceRepo   = "репозиторий"
	projectSourceModel  = "модель"
)

const (
	modelRepoLabel        = "Репозиторий: "
	modelDescriptionLabel = "Описание: "
)

// modelSourceNote* - значения source, по которым projectSourceNote выбирает
// ответ; отдельные от projectSource* деклараций - B после переноса байт в
// байт, отдельные литералы не сливаются в общую константу с A.
const (
	modelSourceNoteModel = "модель"
	modelSourceNoteRepo  = "репозиторий"
)

// projectSourceNote - откуда взялось описание проекта: контекст уходит в
// инструкцию интервью и определяет вопросы по всем будущим обращениям проекта,
// поэтому придуманное моделью автор должен отличать от своего.
func projectSourceNote(source string) string {
	switch source {
	case modelSourceNoteModel:
		return "Название и описание я собрал по README - проверьте, так ли это."
	case modelSourceNoteRepo:
		return "Описание модель собрать не смогла, взял из полей репозитория."
	default:
		return "Название и описание ваши, я их не трогал."
	}
}

// Unclear - строка о незакрытом ядре для саммари и тела тикета: «Не уточнено:
// конкретный случай, что нужно.». Ядро закрыто - пусто. Счёт по Missing, а не
// по gaps модели: строку, от которой зависит метка неполноты, считает Go.
func (c Contract) Unclear(kind string, filled map[string]string) string {
	var titles []string
	for _, key := range c.Missing(kind, filled) {
		titles = append(titles, lowerFirst(c.Title(kind, key)))
	}
	if len(titles) == 0 {
		return ""
	}
	return "Не уточнено: " + strings.Join(titles, ", ") + "."
}
