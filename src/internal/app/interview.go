package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	tele "gopkg.in/telebot.v4"
)

const (
	stepInterview = "interview"
	stepSummary   = "summary"
	// Больше трёх вопросов за раз человек не читает, а отвечает на первый.
	maxQuestions = 3
	// Сколько раз один пункт контракта вообще может стать вопросом. Раунд
	// задаёт несколько вопросов, а человек отвечает одной репликой на один из
	// них - остальные модель законно спрашивает снова. Второй заход уместен,
	// третий автор читает как испорченную пластинку.
	maxAsks  = 2
	maxTitle = 80
)

var (
	interviewPrompt = mustPrompt("interview.md")
	summaryPrompt   = mustPrompt("summary.md")

	ErrNotInterview = errors.New("case is not in interview")
	ErrStaleRound   = errors.New("round is not current")
	// Ответ по текущему раунду уже принят: кнопку нажали второй раз, пока ход
	// ещё думает. Для автора это не «прошлый вопрос», а тот же самый.
	ErrRoundAnswered = errors.New("round is already answered")
	ErrNoSummary     = errors.New("case has no summary to confirm")
	ErrNoSuggestion  = errors.New("round has no suggestions to accept")
)

// Interview - шаги разговора: добивание контракта раундами вопросов и сборка
// саммари. Побочные эффекты выполняет Go: модель возвращает структуру, Go
// валидирует её и только потом пишет в БД.
type Interview struct {
	cases  *Cases
	llm    *OpenRouter
	log    *slog.Logger
	rules  Contract
	model  DialogModel
	rounds int

	// Готовые куски системного сообщения: собираются один раз, потому что
	// стабильный префикс не имеет права меняться между вызовами.
	askPrefix  string
	sumPrefix  string
	turnSchema json.RawMessage
}

func NewInterview(cases *Cases, llm *OpenRouter, log *slog.Logger, rules Contract, model DialogModel, rounds int) *Interview {
	return &Interview{
		cases:      cases,
		llm:        llm,
		log:        log,
		rules:      rules,
		model:      model,
		rounds:     rounds,
		askPrefix:  strings.ReplaceAll(interviewPrompt, "{{CONTRACT}}", rules.Prompt()),
		sumPrefix:  strings.ReplaceAll(summaryPrompt, "{{CONTRACT}}", rules.Prompt()),
		turnSchema: turnSchema(rules),
	}
}

type keyValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Question - вопрос раунда. Suggested - догадка модели: автор подтверждает её
// одной кнопкой, и это экономит ему раунд переписки.
type Question struct {
	Key       string `json:"key"`
	Text      string `json:"text"`
	Suggested string `json:"suggested"`
}

type interviewTurn struct {
	Kind      string     `json:"kind"`
	Filled    []keyValue `json:"filled"`
	Gaps      []string   `json:"gaps"`
	Questions []Question `json:"questions"`
	Ready     bool       `json:"ready"`
}

// section - раздел саммари: ключ пункта контракта и текст. Заголовок берётся из
// правил, а не от модели: тикет одного типа должен выглядеть одинаково.
type section struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

type summaryOut struct {
	Title    string    `json:"title"`
	Sections []section `json:"sections"`
}

// turnSchema строится из правил: список типов обращения задаётся ими же, и
// захардкоженный enum разошёлся бы с контрактом при первой правке.
func turnSchema(rules Contract) json.RawMessage {
	kinds, err := json.Marshal(kindList(rules))
	if err != nil {
		panic("interview schema: " + err.Error())
	}
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"kind": {"type": "string", "enum": ` + string(kinds) + `},
			"filled": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {"key": {"type": "string"}, "value": {"type": "string"}},
					"required": ["key", "value"],
					"additionalProperties": false
				}
			},
			"gaps": {"type": "array", "items": {"type": "string"}},
			"questions": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"key": {"type": "string"},
						"text": {"type": "string"},
						"suggested": {"type": "string"}
					},
					"required": ["key", "text", "suggested"],
					"additionalProperties": false
				}
			},
			"ready": {"type": "boolean"}
		},
		"required": ["kind", "filled", "gaps", "questions", "ready"],
		"additionalProperties": false
	}`)
}

var summarySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"title": {"type": "string"},
		"sections": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {"key": {"type": "string"}, "text": {"type": "string"}},
				"required": ["key", "text"],
				"additionalProperties": false
			}
		}
	},
	"required": ["title", "sections"],
	"additionalProperties": false
}`)

// turnsCount - версия разговора: сколько ответов автора он уже вобрал. Ход
// читает её перед вызовом модели и требует неизменности при записи. Пока модель
// думает, автор может дописать - тогда работа хода уже заменена новой, и
// устаревший результат не имеет права лечь в базу поверх свежего.
func (c *Cases) turnsCount(ctx context.Context, db txRunner, caseID string) (int, error) {
	var n int
	err := db.QueryRow(ctx, `
		SELECT count(*) FROM case_events
		WHERE case_id = $1 AND kind = 'answer_given'`, caseID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count answers of case %s: %w", caseID, err)
	}
	return n, nil
}

// isFix - ход идёт после показанного саммари, то есть автор прислал правку.
// Признак выводится из журнала, а не переносится в payload работы: работу
// заменяет каждое следующее сообщение автора, и признак в ней терялся бы на
// втором сообщении подряд.
func (c *Cases) isFix(ctx context.Context, caseID string) (bool, error) {
	var kind string
	err := c.pool.QueryRow(ctx, `
		SELECT kind FROM case_events
		WHERE case_id = $1 AND kind IN ('round_asked', 'summary_ready')
		ORDER BY id DESC LIMIT 1`, caseID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check fix of case %s: %w", caseID, err)
	}
	return kind == "summary_ready", nil
}

// Run - один ход интервью: спросить модель, что уже собрано и чего не хватает,
// и либо задать раунд вопросов, либо перейти к саммари.
func (i *Interview) Run(ctx context.Context, job Job) error {
	var p casePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return fmt.Errorf("payload of %s: %w", job.Kind, err)
	}

	cs, err := i.cases.Load(ctx, p.CaseID)
	if err != nil {
		return err
	}
	// Обращение отменили или разговор ушёл дальше: работа устарела.
	if cs == nil || cs.Status != statusInterview {
		return nil
	}
	if cs.ProjectID == nil {
		return fmt.Errorf("case %s has no project", cs.ID)
	}

	version, err := i.cases.turnsCount(ctx, i.cases.pool, cs.ID)
	if err != nil {
		return err
	}
	fix, err := i.cases.isFix(ctx, cs.ID)
	if err != nil {
		return err
	}

	messages, err := i.dialog(ctx, cs, i.askPrefix)
	if err != nil {
		return err
	}

	turn, err := i.askTurn(ctx, cs, messages)
	if err != nil {
		return err
	}

	// Исчерпанный пункт снимается с вопросов и остаётся пробелом: тикет уйдёт с
	// пометкой о неполноте, и это честнее третьего повтора. Правка саммари
	// предела не знает, как и предела раундов: автор пришёл уточнять именно
	// этот пункт, и молчание в ответ обесценило бы правку.
	if !fix {
		asked, err := i.cases.askedKeys(ctx, cs.ID)
		if err != nil {
			return err
		}
		kept := slices.DeleteFunc(turn.Questions, func(q Question) bool { return asked[q.Key] >= maxAsks })
		if len(kept) < len(turn.Questions) {
			i.log.Info("questions_exhausted", "case_id", cs.ID, "dropped", len(turn.Questions)-len(kept))
		}
		turn.Questions = kept
	}

	// Предел считается по уже заданным раундам: исчерпав их, ход не спрашивает
	// ничего, а собирает саммари с тем, что есть. Правка саммари предел не
	// проверяет - иначе автор, заметивший ошибку на последнем раунде, не может
	// её исправить.
	toSummary := turn.Ready || len(turn.Questions) == 0 || (!fix && cs.Round >= i.rounds)
	round := cs.Round
	if !toSummary {
		round++
	}
	filled := i.mergeFilled(cs.Filled, turn)
	// Смена типа обращения снимает ключи чужого контракта: собранные ответы
	// исчезают из состояния. Промт менять тип без повода запрещает, но проверить
	// повод нечем, а вот увидеть саму пропажу обязаны - иначе разговор
	// необъяснимо начинает спрашивать заново.
	if cs.Kind != "" && cs.Kind != turn.Kind {
		i.log.Warn("case_kind_changed", "case_id", cs.ID, "from", cs.Kind, "to", turn.Kind,
			"lost_keys", strings.Join(lostKeys(cs.Filled, filled), ","))
	}

	saved, err := i.saveTurn(ctx, cs, turn, filled, round, toSummary, version)
	if err != nil {
		return err
	}
	if !saved {
		// Автор дописал, пока модель думала: его ответ уже поставил свежий ход,
		// и этот результат устарел целиком.
		i.log.Info("interview_turn_stale", "case_id", cs.ID, "round", cs.Round)
		return nil
	}

	// Ключи пробелов, а не только их число: решение «оставлять ли пункт
	// обязательным» принимается по тому, какой из них не закрывается чаще
	// прочих, и по счётчику этого не увидеть. Ключ - имя пункта контракта,
	// содержимого обращения в нём нет.
	i.log.Info("interview_round", "case_id", cs.ID, "round", round, "kind", turn.Kind,
		"questions", len(turn.Questions), "gaps", len(turn.Gaps),
		"gap_keys", strings.Join(turn.Gaps, ","), "to_summary", toSummary)
	return nil
}

// saveTurn кладёт ход разговора: состояние контракта, событие раунда и то, что
// уходит автору либо в следующую работу. Одной транзакцией - иначе вопрос
// уходит автору, а раунд в базе не сохранён.
// Второе значение - лёг ли результат в базу. Ложь означает, что ход устарел:
// обращение отменили или автор дописал, пока модель думала.
func (i *Interview) saveTurn(ctx context.Context, cs *Case, turn interviewTurn, filled map[string]string, round int, toSummary bool, version int) (bool, error) {
	saved := false
	err := i.cases.inTx(ctx, func(tx pgx.Tx) error {
		// Версия разговора сверяется внутри той же транзакции: между её чтением
		// и записью автор мог прислать ещё один ответ, и тогда писать этот ход
		// поверх свежего нельзя.
		current, err := i.cases.turnsCount(ctx, tx, cs.ID)
		if err != nil {
			return err
		}
		if current != version {
			return nil
		}

		contract, err := json.Marshal(filled)
		if err != nil {
			return fmt.Errorf("encode contract of case %s: %w", cs.ID, err)
		}
		gaps, err := json.Marshal(turn.Gaps)
		if err != nil {
			return fmt.Errorf("encode gaps of case %s: %w", cs.ID, err)
		}

		tag, err := tx.Exec(ctx, `
			UPDATE cases SET kind = $2, contract = $3, gaps = $4, round = $5, updated_at = now()
			WHERE id = $1 AND status = 'interview'`, cs.ID, turn.Kind, contract, gaps, round)
		if err != nil {
			return fmt.Errorf("save turn of case %s: %w", cs.ID, err)
		}
		// Обращение отменили, пока модель думала: ни вопроса, ни саммари.
		if tag.RowsAffected() == 0 {
			return nil
		}
		saved = true

		if toSummary {
			if err := addEvent(ctx, tx, cs.ID, "interview_done", map[string]any{
				"round": round, "gaps": turn.Gaps,
			}); err != nil {
				return err
			}
			return replaceJob(ctx, tx, JobSummarize, cs.ID, casePayload{CaseID: cs.ID})
		}

		if err := addEvent(ctx, tx, cs.ID, "round_asked", map[string]any{
			"round": round, "questions": turn.Questions,
		}); err != nil {
			return err
		}
		// Кнопка идёт только под раундом, где есть догадки: обещание подтвердить
		// их одним нажатием обязано совпадать с тем, что автор видит в тексте.
		keys := keysAsk
		if hasSuggestion(turn.Questions) {
			keys = keysRound
		}
		return putNotifyKey(ctx, tx, cs.ID, fmt.Sprintf("round-%d", round),
			roundMessage(turn.Questions), keys)
	})
	return saved, err
}

// askTurn спрашивает модель и проверяет её ответ. Невалидный ответ - один
// повтор: модель промахивается разово, второй такой же промах означает, что
// дело не в случайности, и работа уходит в повтор очередью.
func (i *Interview) askTurn(ctx context.Context, cs *Case, messages []Message) (interviewTurn, error) {
	req := Request{
		Step:       stepInterview,
		Model:      i.model.Name,
		Reasoning:  i.model.Reasoning,
		MaxTokens:  llmMaxTokens,
		Messages:   messages,
		SchemaName: "interview_turn",
		Schema:     i.turnSchema,
	}

	var lastErr error
	// Ход без единой догадки годен, но стоит автору лишних минут: кнопке нечего
	// подтверждать. Тратим на догадки первую попытку из двух, а ход держим:
	// второй заход может кончиться и невалидным ответом, и терять из-за этого
	// готовые вопросы дороже, чем отдать раунд без кнопки.
	var noSuggestion *interviewTurn
	for attempt := 0; attempt < 2; attempt++ {
		raw, err := i.llm.Complete(ctx, req)
		if err != nil {
			return interviewTurn{}, err
		}

		var turn interviewTurn
		if err := json.Unmarshal(raw, &turn); err != nil {
			lastErr = fmt.Errorf("decode turn: %w", err)
		} else if err := i.checkTurn(cs.Filled, turn); err != nil {
			lastErr = err
		} else if attempt == 0 && len(turn.Questions) > 0 && !hasSuggestion(turn.Questions) {
			i.log.Warn("turn_without_suggestion", "step", stepInterview, "case_id", cs.ID,
				"questions", len(turn.Questions))
			noSuggestion = &turn
			continue
		} else {
			return turn, nil
		}
		i.log.Warn("llm_invalid", "step", stepInterview, "case_id", cs.ID,
			"attempt", attempt+1, "error", lastErr)
	}
	if noSuggestion != nil {
		return *noSuggestion, nil
	}
	return interviewTurn{}, fmt.Errorf("interview turn of case %s: %w", cs.ID, lastErr)
}

// hasSuggestion - в раунде есть что подтверждать кнопкой. Вопрос без догадки
// кнопка не закрывает (AcceptRound его пропускает), поэтому раунд из одних
// таких вопросов не должен ни обещать подтверждение, ни показывать кнопку.
func hasSuggestion(questions []Question) bool {
	return slices.ContainsFunc(questions, func(q Question) bool { return !isStub(q.Suggested) })
}

// mergeFilled - состояние контракта после хода: накопленное прошлыми раундами
// плюс свежее. Ключ в gaps переоткрывает пункт, смена типа обращения снимает
// ключи чужого контракта.
func (i *Interview) mergeFilled(prior map[string]string, turn interviewTurn) map[string]string {
	filled := make(map[string]string, len(prior)+len(turn.Filled))
	for key, value := range prior {
		if i.rules.Title(turn.Kind, key) != "" {
			filled[key] = value
		}
	}
	for _, kv := range turn.Filled {
		filled[kv.Key] = strings.TrimSpace(kv.Value)
	}
	for _, key := range turn.Gaps {
		delete(filled, key)
	}
	return filled
}

// lostKeys - пункты, которые были закрыты и после хода закрытыми быть
// перестали. Имена пунктов контракта, содержимого обращения в них нет.
func lostKeys(prior, filled map[string]string) []string {
	var lost []string
	for key := range prior {
		if _, ok := filled[key]; !ok {
			lost = append(lost, key)
		}
	}
	slices.Sort(lost)
	return lost
}

// checkTurn - проверки недоверенного вывода модели. Схема гарантирует форму, а
// смысл проверяет Go: ключи вне контракта, вопрос про закрытый пункт и
// готовность при незакрытых обязательных пунктах прошли бы схему насквозь.
// Обязательность считается по слитому состоянию: контракт копится, и пункт,
// закрытый прошлым раундом, модель повторять не обязана.
func (i *Interview) checkTurn(prior map[string]string, turn interviewTurn) error {
	items := i.rules.Items(turn.Kind)
	if len(items) == 0 {
		return fmt.Errorf("unknown case kind %q", turn.Kind)
	}
	if len(turn.Questions) > maxQuestions {
		return fmt.Errorf("turn has %d questions", len(turn.Questions))
	}

	for _, kv := range turn.Filled {
		if i.rules.Title(turn.Kind, kv.Key) == "" {
			return fmt.Errorf("filled key %q is not in contract", kv.Key)
		}
	}
	for _, key := range turn.Gaps {
		if i.rules.Title(turn.Kind, key) == "" {
			return fmt.Errorf("gap key %q is not in contract", key)
		}
	}
	// Два вопроса об одном пункте в одном раунде сожгли бы его предел за раз:
	// счётчик заданных вопросов считает по журналу, а не по раундам.
	seen := make(map[string]bool, len(turn.Questions))
	for _, q := range turn.Questions {
		if !slices.Contains(turn.Gaps, q.Key) {
			return fmt.Errorf("question about closed key %q", q.Key)
		}
		if strings.TrimSpace(q.Text) == "" {
			return fmt.Errorf("question about %q is empty", q.Key)
		}
		if seen[q.Key] {
			return fmt.Errorf("two questions about key %q", q.Key)
		}
		// Отписку вместо догадки промт запрещает прямо, а ловил её только
		// момент нажатия «Всё так» - автор к тому времени уже прочитал
		// «Предполагаю: не указано» и потерял доверие к кнопке.
		if strings.TrimSpace(q.Suggested) != "" && isStub(q.Suggested) {
			return fmt.Errorf("question about %q suggests a stub", q.Key)
		}
		seen[q.Key] = true
	}
	// Готовность держат только обязательные пункты. Необязательный остаётся в
	// gaps и уходит в тикет строкой «не разобрано»: требовать пустой gaps
	// значило бы либо не давать разговору закончиться, либо заставлять модель
	// прятать непрочитанное - именно на этом противоречии контур выбрасывал
	// готовые генерации (наблюдение 2026-08-12).
	missing := i.rules.Missing(turn.Kind, i.mergeFilled(prior, turn))
	if turn.Ready && len(missing) > 0 {
		return fmt.Errorf("turn is ready with %d required gaps", len(missing))
	}
	// Готовность обрывает разговор, и заданные тем же ходом вопросы автору уже
	// не уйдут. Раньше это исключалось само собой (готовность требовала пустых
	// gaps, а вопрос - ключа из них); теперь необязательный пункт остаётся в
	// gaps, и модель может спросить про него, объявив разговор законченным.
	if turn.Ready && len(turn.Questions) > 0 {
		return fmt.Errorf("turn is ready with %d questions", len(turn.Questions))
	}
	// Иначе разговор встаёт: не готово, а спросить нечего.
	if !turn.Ready && len(turn.Questions) == 0 {
		return errors.New("turn is not ready and has no questions")
	}
	// Обязательный пункт, не закрытый и не названный пробелом, ушёл бы в тикет
	// молчанием. Признаваться в непрочитанном модель обязана.
	for _, key := range missing {
		if !slices.Contains(turn.Gaps, key) {
			return fmt.Errorf("required key %q is neither filled nor in gaps", key)
		}
	}
	return nil
}

// Summarize собирает саммари и показывает его автору. Это последняя точка, где
// ловится неверно прочитанный скриншот: файлы ещё не удалены.
func (i *Interview) Summarize(ctx context.Context, job Job) error {
	var p casePayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return fmt.Errorf("payload of %s: %w", job.Kind, err)
	}

	cs, err := i.cases.Load(ctx, p.CaseID)
	if err != nil {
		return err
	}
	if cs == nil || cs.Status != statusInterview {
		return nil
	}
	if cs.ProjectID == nil {
		return fmt.Errorf("case %s has no project", cs.ID)
	}

	version, err := i.cases.turnsCount(ctx, i.cases.pool, cs.ID)
	if err != nil {
		return err
	}

	messages, err := i.dialog(ctx, cs, i.sumPrefix)
	if err != nil {
		return err
	}

	out, err := i.askSummary(ctx, cs, messages)
	if err != nil {
		return err
	}

	title := scrubContacts(strings.TrimSpace(out.Title))
	body := i.renderSections(cs, out.Sections)
	// Недобран контракт или нет, решают обязательные пункты: необязательный
	// пробел честно назван в теле тикета, но метки о неполноте не заслуживает -
	// иначе её носил бы каждый тикет.
	incomplete := len(i.rules.Missing(cs.Kind, cs.Filled)) > 0
	// Ни одной строки ни от модели, ни из контракта: показывать автору нечего,
	// и работа уходит в повторы, а исчерпав их - скажет ему об этом.
	if body == "" {
		return fmt.Errorf("summary of case %s has no content", cs.ID)
	}

	moved := false
	err = i.cases.inTx(ctx, func(tx pgx.Tx) error {
		// Автор дописал, пока собиралось саммари: его ответ уже поставил новый
		// ход интервью, и показывать саммари без этой правки нельзя.
		current, err := i.cases.turnsCount(ctx, tx, cs.ID)
		if err != nil {
			return err
		}
		if current != version {
			return nil
		}

		tag, err := tx.Exec(ctx, `
			UPDATE cases SET status = 'summary', title = $2, summary = $3, incomplete = $4,
			                 updated_at = now()
			WHERE id = $1 AND status = 'interview'`, cs.ID, title, body, incomplete)
		if err != nil {
			return fmt.Errorf("save summary of case %s: %w", cs.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		moved = true

		// Текст саммари ложится снимком в событие, а не читается потом из
		// колонки: колонка держит только последнюю версию, а цепочку правок
		// модель обязана видеть целиком.
		if err := addEvent(ctx, tx, cs.ID, "summary_ready", map[string]any{
			"incomplete": incomplete, "sections": len(out.Sections),
			"title": title, "body": body,
		}); err != nil {
			return err
		}
		// Ключ по работе, а не по раунду: правка саммари раунд не двигает, и
		// переписанное саммари упёрлось бы в ключ прошлого - автор не увидел бы
		// собственную правку.
		return putNotifyKey(ctx, tx, cs.ID, strconv.FormatInt(job.ID, 10),
			summaryMessage(title, body, i.gapTitles(cs), incomplete), keysSummary)
	})
	if err != nil {
		return err
	}
	if !moved {
		return nil
	}

	i.log.Info("summary_ready", "case_id", cs.ID, "incomplete", incomplete,
		"gap_keys", strings.Join(cs.Gaps, ","),
		"sections", len(out.Sections), "chars", utf8.RuneCountInString(body))
	return nil
}

func (i *Interview) askSummary(ctx context.Context, cs *Case, messages []Message) (summaryOut, error) {
	req := Request{
		Step:       stepSummary,
		Model:      i.model.Name,
		Reasoning:  i.model.Reasoning,
		MaxTokens:  llmMaxTokens,
		Messages:   messages,
		SchemaName: "case_summary",
		Schema:     summarySchema,
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		raw, err := i.llm.Complete(ctx, req)
		if err != nil {
			return summaryOut{}, err
		}

		var out summaryOut
		if err := json.Unmarshal(raw, &out); err != nil {
			lastErr = fmt.Errorf("decode summary: %w", err)
		} else if err := i.checkSummary(cs, out); err != nil {
			lastErr = err
		} else {
			return out, nil
		}
		i.log.Warn("llm_invalid", "step", stepSummary, "case_id", cs.ID,
			"attempt", attempt+1, "error", lastErr)
	}
	return summaryOut{}, fmt.Errorf("summary of case %s: %w", cs.ID, lastErr)
}

func (i *Interview) checkSummary(cs *Case, out summaryOut) error {
	title := strings.TrimSpace(out.Title)
	if title == "" {
		return errors.New("summary has no title")
	}
	if utf8.RuneCountInString(title) > maxTitle {
		return fmt.Errorf("summary title is %d runes long", utf8.RuneCountInString(title))
	}
	// Заголовок «Проблема: не сохраняется форма» тратит место списка на слово,
	// которое и так известно из типа тикета. Промт это запрещает, проверять
	// было некому.
	if word := strings.ToLower(strings.Fields(title)[0]); slices.Contains(titleStopWords, strings.Trim(word, ":-")) {
		return fmt.Errorf("summary title starts with %q", word)
	}

	for _, s := range out.Sections {
		if i.rules.Title(cs.Kind, s.Key) == "" {
			return fmt.Errorf("section key %q is not in contract", s.Key)
		}
		if strings.TrimSpace(s.Text) == "" {
			return fmt.Errorf("section %q is empty", s.Key)
		}
	}
	return nil
}

// renderSections собирает тело саммари в markdown - тот же текст уходит и в
// issue, и автору. Порядок разделов задают правила, а не ответ модели: тикет
// одного типа выглядит одинаково. Раздел, который модель не написала,
// достраивается из контракта - почему так, раздел 7 architecture.md.
func (i *Interview) renderSections(cs *Case, sections []section) string {
	texts := make(map[string]string, len(sections))
	for _, s := range sections {
		// Пробел остаётся пробелом: раздел по незакрытому пункту - догадка,
		// которой автор не давал, а сообщение о пробелах тут же ей противоречит.
		if slices.Contains(cs.Gaps, s.Key) {
			continue
		}
		texts[s.Key] = scrubContacts(strings.TrimSpace(s.Text))
	}
	for key, value := range cs.Filled {
		if texts[key] != "" || slices.Contains(cs.Gaps, key) {
			continue
		}
		texts[key] = scrubContacts(strings.TrimSpace(value))
	}

	var b strings.Builder
	for _, item := range i.rules.Items(cs.Kind) {
		text := texts[item.Key]
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", item.Title, text)
	}
	return strings.TrimSpace(b.String())
}

// gapTitles - незакрытые пункты человеческими названиями. Автор видит их до
// публикации: недобранный контракт даёт тикет с пометкой, а не отказ.
func (i *Interview) gapTitles(cs *Case) []string {
	var titles []string
	for _, key := range cs.Gaps {
		if title := i.rules.Title(cs.Kind, key); title != "" {
			titles = append(titles, title)
		}
	}
	return titles
}

// dialog собирает сообщения запроса. Порядок обязателен: стабильный префикс
// первым сообщением, протокол сырья вторым, история раундов последней. Любая
// изменяющаяся строка перед промтом молча гасит кэш провайдера.
func (i *Interview) dialog(ctx context.Context, cs *Case, prefix string) ([]Message, error) {
	project, err := LoadProject(ctx, i.cases.pool, *cs.ProjectID)
	if err != nil {
		return nil, err
	}

	messages := []Message{
		{Role: "system", Parts: []Part{TextPart(prefix + "\n\n## Проект\n\n" + project.Context)}},
		{Role: "user", Parts: []Part{TextPart("Протокол сырья:\n\n" + cs.Protocol)}},
	}

	history, err := i.cases.history(ctx, cs.ID)
	if err != nil {
		return nil, err
	}
	return append(messages, history...), nil
}

// history восстанавливает разговор из журнала. Отдельной таблицы у него нет:
// диалог по природе append-only, а case_events уже пишется в тех же
// транзакциях, что и смена статуса. Показанное саммари - такая же реплика бота,
// как вопрос раунда: автор правит именно его.
func (c *Cases) history(ctx context.Context, caseID string) ([]Message, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT kind, payload FROM case_events
		WHERE case_id = $1 AND kind IN ('round_asked', 'answer_given', 'summary_ready')
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
		case "answer_given":
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(payload, &p); err != nil {
				return nil, fmt.Errorf("decode given answer: %w", err)
			}
			messages = append(messages, Message{Role: "user", Parts: []Part{TextPart(p.Text)}})
		case "summary_ready":
			var p struct {
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			if err := json.Unmarshal(payload, &p); err != nil {
				return nil, fmt.Errorf("decode shown summary: %w", err)
			}
			// Обращение начато до выката: снимка в событии нет, и подставить
			// вместо него нечего.
			if p.Body == "" {
				continue
			}
			messages = append(messages, Message{
				Role:  "assistant",
				Parts: []Part{TextPart(p.Title + "\n\n" + p.Body)},
			})
		}
	}
	return messages, rows.Err()
}

// AddAnswer принимает ответ автора: текстом, расшифровкой голосового или
// подтверждением раунда. Живёт рядом с состоянием, а не с моделью: ответ надо
// сохранить и поставить следующий ход, а спрашивать модель будет работа.
//
// Ответ при показанном саммари - это правка: обращение возвращается в интервью
// и получает ход без проверки предела раундов.
func (c *Cases) AddAnswer(ctx context.Context, cs *Case, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if cs.Status != statusInterview && cs.Status != statusSummary {
		return ErrNotInterview
	}

	moved := false
	err := c.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE cases SET status = 'interview', updated_at = now()
			WHERE id = $1 AND status IN ('interview', 'summary')`, cs.ID)
		if err != nil {
			return fmt.Errorf("accept answer of case %s: %w", cs.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		moved = true

		if err := addEvent(ctx, tx, cs.ID, "answer_given", map[string]any{
			"round": cs.Round, "text": text,
		}); err != nil {
			return err
		}

		// Человек дописывает вторым сообщением: замена снимает ещё не начатый
		// ход, и модель отвечает один раз на всё сразу. Правка это или обычный
		// ответ, решает сам ход по журналу - в работе этот признак терялся бы
		// при замене.
		return replaceJob(ctx, tx, JobInterview, cs.ID, casePayload{CaseID: cs.ID})
	})
	if err != nil {
		return err
	}
	if !moved {
		return ErrNotInterview
	}

	cs.Status = statusInterview
	c.log.Info("answer_given", "case_id", cs.ID, "round", cs.Round,
		"chars", utf8.RuneCountInString(text))
	return nil
}

// AcceptRound - кнопка «Всё так»: предположения модели становятся ответом
// целиком. Номер раунда приходит из callback_data и сверяется с текущим: кнопка
// прошлого раунда осталась в чате и не должна закрывать чужие вопросы.
func (c *Cases) AcceptRound(ctx context.Context, cs *Case, round int) error {
	if cs.Status != statusInterview {
		return ErrNotInterview
	}
	if round != cs.Round {
		return ErrStaleRound
	}

	// На раунд уже отвечали: кнопка нажата второй раз, пока первый ход ещё
	// думает. Номер раунда этого не ловит - он меняется только следующим ходом,
	// поэтому смотрим, что было последним событием разговора.
	answered, err := c.roundAnswered(ctx, cs.ID)
	if err != nil {
		return err
	}
	if answered {
		return ErrRoundAnswered
	}

	questions, err := c.lastQuestions(ctx, cs.ID)
	if err != nil {
		return err
	}
	if len(questions) == 0 {
		return ErrStaleRound
	}

	// Вопрос без догадки кнопка не закрывает: отписка стала бы подтверждённым
	// ответом, которого автор не видел. Такой пункт остаётся открытым.
	var b strings.Builder
	for _, q := range questions {
		if isStub(q.Suggested) {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", q.Text, strings.TrimSpace(q.Suggested))
	}
	if b.Len() == 0 {
		return ErrNoSuggestion
	}
	return c.AddAnswer(ctx, cs, b.String())
}

// isStub - догадки нет: пусто или отписка вроде «не указано». Промт такие
// строки запрещает, но кнопка «Всё так» превращает догадку в слова автора, а
// выдуманный ответ дороже лишнего вопроса: сомнительную строку лучше не
// принять, чем принять.
func isStub(text string) bool {
	text = strings.ToLower(strings.Trim(text, " .,:;-"))
	if text == "" {
		return true
	}
	// Отписка на этих словах обрывается: «дата не указана». Тот же оборот
	// внутри фразы - факт о сервисе: «в заказе не указан адрес».
	for _, tail := range stubTails {
		if strings.HasSuffix(text, tail) {
			return true
		}
	}
	return slices.ContainsFunc(stubPhrases, func(p string) bool { return strings.Contains(text, p) })
}

var (
	stubTails = []string{"не указано", "не указан", "не указана", "не указаны", "неизвестно",
		"не известно", "не разобрано", "неясно", "не ясно", "не сообщил", "не сообщила"}
	stubPhrases = []string{"нет данных", "данных нет", "нет информации", "информации нет",
		"информация отсутствует", "данные отсутствуют", "не удалось определить",
		"уточнить не удалось", "не сообщается"}
)

// roundAnswered - последним событием разговора идёт ответ, а не вопрос. Значит
// текущий раунд закрыт и подтверждать в нём нечего.
func (c *Cases) roundAnswered(ctx context.Context, caseID string) (bool, error) {
	var kind string
	err := c.pool.QueryRow(ctx, `
		SELECT kind FROM case_events
		WHERE case_id = $1 AND kind IN ('round_asked', 'answer_given')
		ORDER BY id DESC LIMIT 1`, caseID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check last event of case %s: %w", caseID, err)
	}
	return kind == "answer_given", nil
}

// askedKeys - сколько раз каждый пункт контракта уже становился вопросом.
// Считается по журналу: раунды переживают рестарт, а память процесса нет.
func (c *Cases) askedKeys(ctx context.Context, caseID string) (map[string]int, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT payload FROM case_events
		WHERE case_id = $1 AND kind = 'round_asked'`, caseID)
	if err != nil {
		return nil, fmt.Errorf("query asked rounds of case %s: %w", caseID, err)
	}
	defer rows.Close()

	asked := map[string]int{}
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("scan asked round: %w", err)
		}
		var p struct {
			Questions []Question `json:"questions"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, fmt.Errorf("decode asked round: %w", err)
		}
		for _, q := range p.Questions {
			asked[q.Key]++
		}
	}
	return asked, rows.Err()
}

func (c *Cases) lastQuestions(ctx context.Context, caseID string) ([]Question, error) {
	var payload []byte
	err := c.pool.QueryRow(ctx, `
		SELECT payload FROM case_events
		WHERE case_id = $1 AND kind = 'round_asked'
		ORDER BY id DESC LIMIT 1`, caseID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load last round of case %s: %w", caseID, err)
	}

	var p struct {
		Questions []Question `json:"questions"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, fmt.Errorf("decode last round of case %s: %w", caseID, err)
	}
	return p.Questions, nil
}

// ConfirmSummary - кнопка «Публикую». Ключ работы без счётчика: issue у
// обращения ровно один, и повторное нажатие обязано упереться в тот же ключ.
func (c *Cases) ConfirmSummary(ctx context.Context, cs *Case) error {
	if cs.Status != statusSummary {
		return ErrNoSummary
	}

	confirmed := false
	err := c.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE cases SET status = 'publishing', updated_at = now()
			WHERE id = $1 AND status = 'summary'`, cs.ID)
		if err != nil {
			return fmt.Errorf("confirm summary of case %s: %w", cs.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		confirmed = true

		if err := addEvent(ctx, tx, cs.ID, "summary_confirmed", nil); err != nil {
			return err
		}

		// Прошлая попытка могла исчерпать повторы и остаться в очереди
		// погашенной: замена возвращает публикацию в работу.
		return replaceJob(ctx, tx, JobPublish, cs.ID, casePayload{CaseID: cs.ID})
	})
	if err != nil {
		return err
	}
	if !confirmed {
		return ErrNoSummary
	}

	cs.Status = statusPublishing
	c.log.Info("summary_confirmed", "case_id", cs.ID, "user_id", cs.UserID)
	return nil
}

// AddVoiceAnswer принимает голосовой ответ на вопрос интервью. Расшифровка
// уходит работой, а не синхронным вызовом: апдейты Telegram обрабатываются
// последовательно, и минута ожидания модели остановила бы бота для всех авторов.
func (c *Cases) AddVoiceAnswer(ctx context.Context, bot *tele.Bot, cs *Case, msg *tele.Message) error {
	if cs.Status != statusInterview && cs.Status != statusSummary {
		return ErrNotInterview
	}
	file := msg.Voice.File
	if file.FileSize > maxFileSize {
		return ErrFileTooBig
	}

	var itemID int64
	err := c.pool.QueryRow(ctx, `
		INSERT INTO case_items (case_id, kind, tg_message_id, tg_file_id, source_text, mime)
		VALUES ($1, 'voice', $2, $3, $4, $5) RETURNING id`,
		cs.ID, msg.ID, file.FileID, msg.Caption, valueOr(msg.Voice.MIME, "audio/ogg")).Scan(&itemID)
	if err != nil {
		return fmt.Errorf("insert answer of case %s: %w", cs.ID, err)
	}

	if err := c.download(ctx, bot, cs, itemID, file); err != nil {
		return err
	}
	key := fmt.Sprintf("%s:%s:%d", JobNormalizeVoice, cs.ID, itemID)
	return PutJob(ctx, c.pool, JobNormalizeVoice, key, itemPayload{CaseID: cs.ID, ItemID: itemID})
}

// AfterVoice решает, куда двигаться после расшифровки. Один и тот же шаг
// нормализации обслуживает и сырьё сбора, и ответ в интервью: сырьё
// превращается в текст одинаково, а что с ним делать - зависит от состояния
// разговора, а не от самой записи.
func (c *Cases) AfterVoice(ctx context.Context, caseID, text string) error {
	cs, err := c.Load(ctx, caseID)
	if err != nil {
		return err
	}
	if cs == nil {
		return nil
	}
	if cs.Status == statusInterview || cs.Status == statusSummary {
		return c.AddAnswer(ctx, cs, text)
	}
	return c.AdvanceNormalize(ctx, caseID)
}

// AfterVoiceFail - та же развилка для нераспознанной записи. В сборе провал
// виден автору строкой протокола, а в разговоре его заметить нечем: молчание
// бота автор читает как «он думает».
func (c *Cases) AfterVoiceFail(ctx context.Context, caseID string, itemID int64) error {
	cs, err := c.Load(ctx, caseID)
	if err != nil {
		return err
	}
	if cs == nil {
		return nil
	}
	if cs.Status == statusInterview || cs.Status == statusSummary {
		return putNotifyKey(ctx, c.pool, caseID, fmt.Sprintf("voicefail-%d", itemID),
			"Не разобрал голосовое. Повторите, пожалуйста, текстом или запишите ещё раз.", "")
	}
	return c.AdvanceNormalize(ctx, caseID)
}

func roundMessage(questions []Question) string {
	tail := "\n\nОтветьте своими словами - текстом или голосовым."
	if hasSuggestion(questions) {
		tail += " Если предположения верны, нажмите «Всё так»."
	}
	return "Уточню, чтобы тикет не пришлось переспрашивать:\n\n" + questionList(questions) + tail
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

func summaryMessage(title, body string, gaps []string, incomplete bool) string {
	var b strings.Builder
	b.WriteString("Вот что уйдёт в тикет.\n\n")
	b.WriteString(title + "\n\n")
	b.WriteString(plainSections(body))
	if len(gaps) > 0 {
		b.WriteString("\n\nОстались пробелы: " + strings.Join(gaps, "; ") + ".")
		// Пометку о неполноте несёт только незакрытый обязательный пункт:
		// обещать её на необязательном пробеле значит пугать автора тем, чего
		// в тикете не будет.
		if incomplete {
			b.WriteString(" Тикет уйдёт с пометкой о неполноте.")
		}
	}
	b.WriteString("\n\nГде я ошибся? Напишите правку - или публикуем.")
	return b.String()
}

// plainSections убирает markdown-заголовки: в тикете они нужны, в Telegram без
// разметки выглядят решётками.
func plainSections(body string) string {
	lines := strings.Split(body, "\n")
	for n, line := range lines {
		lines[n] = strings.TrimPrefix(line, "## ")
	}
	return strings.Join(lines, "\n")
}

// Структурные персональные данные вырезаются детерминированно до записи в
// тикет; остальное (ФИО, переписка) - забота промтов, последняя защита - автор
// видит саммари. Шаблоны намеренно узкие: широкий съел бы номера заказов.
var (
	emailRe = regexp.MustCompile(`[\p{L}\d._%+-]+@[\p{L}\d.-]+\.[\p{L}]{2,}`)
	// Телефон опознаётся по форме, а не по длине: либо разделители внутри
	// номера, либо одиннадцать цифр с 7 или 8 в начале. Голая цепочка цифр
	// телефоном не считается - это номер заказа.
	phoneRe = regexp.MustCompile(`(?:\+\d{1,3}[\s(-]?)?\d{3}[\s)-]\d{3}[\s-]\d{2}[\s-]\d{2}|\b[78]\d{10}\b`)
	cardRe  = regexp.MustCompile(`\b\d{4}[\s-]\d{4}[\s-]\d{4}[\s-]\d{4}\b`)
)

func scrubContacts(text string) string {
	text = emailRe.ReplaceAllString(text, "[почта]")
	text = cardRe.ReplaceAllString(text, "[карта]")
	return phoneRe.ReplaceAllString(text, "[телефон]")
}

// titleStopWords - служебные слова, с которых заголовок начинать нельзя: тип
// тикета виден по метке, а в списке видно только заголовок.
var titleStopWords = []string{"проблема", "баг", "ошибка", "просьба", "вопрос", "запрос"}

func kindList(rules Contract) []string {
	kinds := make([]string, 0, len(rules))
	for _, kind := range caseKinds {
		if len(rules[kind]) > 0 {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}
