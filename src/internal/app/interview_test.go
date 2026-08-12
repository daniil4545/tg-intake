package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testRules(t *testing.T) Contract {
	t.Helper()

	rules, err := LoadContract()
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	return rules
}

func newTestInterview(t *testing.T, cases *Cases, rounds int) *Interview {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewInterview(cases, nil, log, testRules(t), "test-model", rounds)
}

// TestLoadContract: правила едут в бинарь и обязаны быть рабочими. Тип без
// единого обязательного пункта означает, что готовым считается любое обращение,
// и интервью не задаст ни одного вопроса - это должно ронять старт, а не
// обнаруживаться на живом диалоге.
func TestLoadContract(t *testing.T) {
	rules := testRules(t)

	for _, kind := range caseKinds {
		if len(rules.Items(kind)) == 0 {
			t.Errorf("тип %q остался без пунктов", kind)
		}
	}
	if rules.Title("bug", "case") == "" {
		t.Error("заголовок пункта bug.case пуст, он же заголовок раздела issue")
	}

	err := checkItems("bug", []ContractItem{{Key: "case", Title: "Случай"}})
	if err == nil {
		t.Error("тип без обязательных пунктов принят, ожидался отказ")
	}
	err = checkItems("bug", []ContractItem{
		{Key: "case", Title: "Случай", Required: true},
		{Key: "case", Title: "Второй раз", Required: true},
	})
	if err == nil {
		t.Error("повтор ключа принят, ожидался отказ")
	}
}

// TestContractHoldsReadiness: состав пунктов правится данными, и правка легко
// отменяет смысл среза. Признак готовности обязан держать готовность пожелания,
// иначе тикет снова описывает желание без границы сделанного; необязательный
// пункт держать её не имеет права - иначе незнание автора станет неполнотой.
func TestContractHoldsReadiness(t *testing.T) {
	rules := testRules(t)

	wish := map[string]string{
		"problem": "выгрузку собирают руками",
		"today":   "копируют строки в таблицу",
		"result":  "кнопка «выгрузить» в списке",
	}
	gaps := rules.Missing("feature", wish)
	if !slices.Contains(gaps, "done") {
		t.Errorf("пожелание без признака готовности считается готовым, пробелы: %v", gaps)
	}
	// Частота нужна для решения «стоит ли автоматизировать», но незнание её не
	// делает обращение неполным: пункт необязателен и в пробелы не попадает.
	if slices.Contains(gaps, "volume") {
		t.Errorf("частота попала в пробелы: %v", gaps)
	}

	bug := map[string]string{
		"case":     "заказ 4821",
		"expected": "статус меняется",
		"actual":   "статус прежний",
	}
	if gaps := rules.Missing("bug", bug); len(gaps) != 0 {
		t.Errorf("необязательный пункт держит готовность бага, пробелы: %v", gaps)
	}

	// Путь до готовности стоит автору вопросов, и правка правил не имеет права
	// удлинить его молча: раундов три, каждый новый обязательный пункт приближает
	// метку неполноты. Держим состав, а не длину: счётчик пропустил бы подмену,
	// снимающую обязательность с «как делают сейчас» ради нового пункта, - а
	// именно эта пара с «каким должен быть результат» и делает тикет исполнимым.
	var required []string
	for _, it := range rules.Items("feature") {
		if it.Required {
			required = append(required, it.Key)
		}
	}
	want := []string{"problem", "today", "result", "done"}
	if !slices.Equal(required, want) {
		t.Errorf("обязательные пункты пожелания: %v, ожидались %v", required, want)
	}
}

// TestCheckTurn: схема гарантирует форму ответа, смысл проверяет Go. Каждое
// нарушение здесь прошло бы схему насквозь и испортило бы тикет молча.
func TestCheckTurn(t *testing.T) {
	i := newTestInterview(t, nil, 3)

	full := []keyValue{
		{Key: "case", Value: "заказ 4821"},
		{Key: "expected", Value: "статус меняется"},
		{Key: "actual", Value: "статус прежний"},
	}

	tests := []struct {
		name  string
		prior map[string]string
		turn  interviewTurn
		ok    bool
	}{
		{
			name: "готовый ход",
			turn: interviewTurn{Kind: "bug", Filled: full, Ready: true},
			ok:   true,
		},
		{
			name:  "обязательный пункт закрыт прошлым раундом",
			prior: map[string]string{"case": "заказ 4821"},
			turn: interviewTurn{
				Kind:      "bug",
				Filled:    full[1:],
				Gaps:      []string{"where"},
				Questions: []Question{{Key: "where", Text: "где смотрели?"}},
			},
			ok: true,
		},
		{
			name: "четыре вопроса",
			turn: interviewTurn{
				Kind: "bug",
				Gaps: []string{"case", "expected", "actual", "where"},
				Questions: []Question{
					{Key: "case", Text: "а"}, {Key: "expected", Text: "б"},
					{Key: "actual", Text: "в"}, {Key: "where", Text: "г"},
				},
			},
		},
		{
			name: "ключ вне контракта",
			turn: interviewTurn{
				Kind:      "bug",
				Gaps:      []string{"deadline"},
				Questions: []Question{{Key: "deadline", Text: "когда нужно?"}},
			},
		},
		{
			name: "готов при незакрытых пробелах",
			turn: interviewTurn{Kind: "bug", Filled: full, Gaps: []string{"where"}, Ready: true},
		},
		{
			name: "обязательный пункт не закрыт и не назван пробелом",
			turn: interviewTurn{
				Kind:      "bug",
				Filled:    full[:1],
				Gaps:      []string{"expected"},
				Questions: []Question{{Key: "expected", Text: "что ожидалось?"}},
			},
		},
		{
			name: "не готов и спросить нечего",
			turn: interviewTurn{Kind: "bug", Filled: full[:1], Gaps: []string{"expected", "actual"}},
		},
		{
			name: "вопрос про закрытый пункт",
			turn: interviewTurn{
				Kind:      "bug",
				Filled:    full,
				Gaps:      []string{"where"},
				Questions: []Question{{Key: "case", Text: "а какой заказ?"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := i.checkTurn(tt.prior, tt.turn)
			if tt.ok && err != nil {
				t.Errorf("ход отклонён: %v", err)
			}
			if !tt.ok && err == nil {
				t.Error("ход принят, ожидался отказ")
			}
		})
	}
}

// TestMergeFilled: контракт копится между раундами. Пункт, не повторённый
// моделью, не пропадает; ключ в gaps переоткрывает пункт; ключ вне контракта
// текущего типа снимается.
func TestMergeFilled(t *testing.T) {
	i := newTestInterview(t, nil, 3)

	prior := map[string]string{
		"case":     "заказ 4821",
		"expected": "статус меняется",
		"чужой":    "ключ другого типа",
	}
	turn := interviewTurn{
		Kind:   "bug",
		Filled: []keyValue{{Key: "actual", Value: " статус прежний "}},
		Gaps:   []string{"expected"},
	}

	got := i.mergeFilled(prior, turn)
	want := map[string]string{"case": "заказ 4821", "actual": "статус прежний"}
	if !maps.Equal(got, want) {
		t.Errorf("слито %v, ожидалось %v", got, want)
	}
}

// TestScrubContacts: структурные персональные данные вырезаются до записи в
// тикет, а идентификаторы карточек остаются - без них тикет бесполезен.
func TestScrubContacts(t *testing.T) {
	in := "Клиент писал с ivan.petrov@example.com и звонил на +7 916 123-45-67, " +
		"карта 4276 3800 1234 5678, заказ 4821000123 в статусе «оплачен»"
	got := scrubContacts(in)

	for _, secret := range []string{"ivan.petrov@example.com", "916 123-45-67", "4276 3800"} {
		if strings.Contains(got, secret) {
			t.Errorf("персональные данные остались: %q в %q", secret, got)
		}
	}
	for _, keep := range []string{"4821000123", "оплачен"} {
		if !strings.Contains(got, keep) {
			t.Errorf("нужное вырезано: %q пропал из %q", keep, got)
		}
	}
}

// TestSummaryWithoutSections: ответ модели без разделов больше не отклоняется.
// Именно эта проверка уводила работу в повторы и оставляла автора без единого
// слова на минуты, пока модель не отвечала «как надо».
func TestSummaryWithoutSections(t *testing.T) {
	i := newTestInterview(t, nil, 3)
	i.llm = fakeLLM(t, `{"title":"Форма не сохраняется","sections":[]}`)
	cs := &Case{ID: "case-1", Kind: "bug", Filled: map[string]string{"case": "заказ 4821"}}

	messages := []Message{{Role: "system", Parts: []Part{TextPart("промт")}}}
	out, err := i.askSummary(context.Background(), cs, messages)
	if err != nil {
		t.Fatalf("саммари без разделов отклонено: %v", err)
	}
	if out.Title != "Форма не сохраняется" {
		t.Errorf("заголовок саммари: %q", out.Title)
	}
}

// TestSectionsFallBackToContract: содержание пунктов уже собрано интервью, и
// молчание модели не имеет права остановить тикет. Раньше такой ответ уходил в
// повторы, а автор ждал в тишине.
func TestSectionsFallBackToContract(t *testing.T) {
	i := newTestInterview(t, nil, 3)
	cs := &Case{
		Kind:   "bug",
		Filled: map[string]string{"case": "заказ 4821", "actual": "статус остался «новый»", "expected": "статус «оплачен»"},
		Gaps:   []string{"expected"},
	}

	body := i.renderSections(cs, nil)

	for _, want := range []string{"заказ 4821", "статус остался «новый»"} {
		if !strings.Contains(body, want) {
			t.Errorf("закрытый пункт потерян: %q\nтело:\n%s", want, body)
		}
	}
	// Пробел остаётся пробелом: недобранное не выдаётся за собранное.
	if strings.Contains(body, "статус «оплачен»") {
		t.Errorf("пробел ушёл в тело саммари:\n%s", body)
	}
}

// TestRenderSections: порядок разделов задают правила, а не ответ модели, и
// заголовки берутся оттуда же. Одно место задаёт и что спрашиваем, и как это
// выглядит в тикете.
func TestRenderSections(t *testing.T) {
	i := newTestInterview(t, nil, 3)

	body := i.renderSections(&Case{Kind: "bug"}, []section{
		{Key: "actual", Text: "статус остался «новый»"},
		{Key: "case", Text: "заказ 4821 от вторника"},
		{Key: "where", Text: ""},
	})

	want := "## Конкретный случай\n\nзаказ 4821 от вторника\n\n" +
		"## Что произошло на самом деле\n\nстатус остался «новый»"
	if body != want {
		t.Errorf("тело саммари:\nполучено:\n%s\nожидалось:\n%s", body, want)
	}
	if strings.Contains(plainSections(body), "## ") {
		t.Error("в сообщении автору остались markdown-заголовки")
	}
}

// startInterview готовит обращение в интервью: тесту нужен разговор, а не
// прогон нормализации.
func startInterview(t *testing.T, cases *Cases, userID int64, round int) *Case {
	t.Helper()
	ctx := context.Background()

	cs, _, err := cases.StartCase(ctx, User{ID: userID, First: "Тест"}, "tg-intake")
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	_, err = cases.pool.Exec(ctx, `
		UPDATE cases SET status = 'interview', kind = 'bug', round = $2,
		                 protocol = 'текст: форма не сохраняется'
		WHERE id = $1`, cs.ID, round)
	if err != nil {
		t.Fatalf("move to interview: %v", err)
	}
	return reload(t, cases, cs.ID)
}

func countJobs(t *testing.T, pool *pgxpool.Pool, kind, caseID string) int {
	t.Helper()

	var n int
	err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM jobs WHERE kind = $1 AND payload->>'case_id' = $2`, kind, caseID).Scan(&n)
	if err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

// TestAcceptRound: «Всё так» принимает раунд целиком, а кнопка прошлого раунда
// не закрывает текущие вопросы - иначе автор подтверждает не то, что видит.
func TestAcceptRound(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6001, 1)

	questions := []Question{
		{Key: "case", Text: "какой заказ?", Suggested: "заказ 4821"},
		{Key: "expected", Text: "что ожидали?", Suggested: "статус меняется на «оплачен»"},
		{Key: "actual", Text: "что вышло?", Suggested: "статус остался «новый»"},
	}
	err := addEvent(ctx, pool, cs.ID, "round_asked", map[string]any{"round": 1, "questions": questions})
	if err != nil {
		t.Fatalf("add round: %v", err)
	}

	if err := cases.AcceptRound(ctx, cs, 0); !errors.Is(err, ErrStaleRound) {
		t.Fatalf("кнопка прошлого раунда: получено %v, ожидалось ErrStaleRound", err)
	}
	if err := cases.AcceptRound(ctx, reload(t, cases, cs.ID), 1); err != nil {
		t.Fatalf("accept round: %v", err)
	}

	history, err := cases.history(ctx, cs.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("история разговора: сообщений %d, ожидалось 2", len(history))
	}
	answer := history[1].Parts[0].text
	for _, q := range questions {
		if !strings.Contains(answer, q.Suggested) {
			t.Errorf("предположение потеряно: %q", q.Suggested)
		}
	}
	if n := countJobs(t, pool, JobInterview, cs.ID); n != 1 {
		t.Errorf("работ интервью после ответа: %d, ожидалась 1", n)
	}

	// Следующий ход идёт секунды, и человек жмёт кнопку ещё раз. Второе нажатие
	// не должно давать ни второго ответа в истории, ни второго хода модели.
	if err := cases.AcceptRound(ctx, reload(t, cases, cs.ID), 1); !errors.Is(err, ErrStaleRound) {
		t.Fatalf("повторное «Всё так»: получено %v, ожидалось ErrStaleRound", err)
	}
	if n := countJobs(t, pool, JobInterview, cs.ID); n != 1 {
		t.Errorf("повтор нажатия добавил работу: работ %d, ожидалась 1", n)
	}
}

// TestAcceptRoundSkipsStub: «Всё так» превращает догадки в слова автора, и
// отписка модели «не указано» стала бы подтверждённым фактом. Такой пункт
// остаётся открытым.
func TestAcceptRoundSkipsStub(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6010, 1)

	questions := []Question{
		{Key: "case", Text: "какой заказ?", Suggested: "заказ 4821"},
		{Key: "actual", Text: "что вышло?", Suggested: "Не указано"},
	}
	err := addEvent(ctx, pool, cs.ID, "round_asked", map[string]any{"round": 1, "questions": questions})
	if err != nil {
		t.Fatalf("add round: %v", err)
	}
	if err := cases.AcceptRound(ctx, reload(t, cases, cs.ID), 1); err != nil {
		t.Fatalf("accept round: %v", err)
	}

	history, err := cases.history(ctx, cs.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	answer := history[len(history)-1].Parts[0].text
	if strings.Contains(strings.ToLower(answer), "не указано") {
		t.Errorf("отписка ушла в ответ автора: %q", answer)
	}
	if !strings.Contains(answer, "заказ 4821") {
		t.Errorf("содержательная догадка потеряна: %q", answer)
	}
}

// TestExhaustedQuestionGoesToSummary: пункт, о котором уже спрашивали дважды,
// третьего вопроса не получает. Ход не задаёт раунд из воздуха, а собирает
// саммари с тем, что есть: повтор одного и того же автор читает как поломку.
func TestExhaustedQuestionGoesToSummary(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6011, 1)

	turn := `{"kind":"bug","filled":[{"key":"case","value":"заказ 4821"}],` +
		`"gaps":["expected","actual"],"ready":false,` +
		`"questions":[{"key":"expected","text":"что ожидали?","suggested":"статус «оплачен»"}]}`
	i := newTestInterview(t, cases, 3)
	i.llm = fakeLLM(t, turn)

	// Два раунда об одном пункте: предел исчерпан, третьего вопроса быть не
	// должно, даже если модель его предлагает.
	for n := 1; n <= maxAsks; n++ {
		err := addEvent(ctx, pool, cs.ID, "round_asked", map[string]any{
			"round": n, "questions": []Question{{Key: "expected", Text: "что ожидали?"}},
		})
		if err != nil {
			t.Fatalf("add round: %v", err)
		}
	}

	job := Job{ID: 1, Kind: JobInterview, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := i.Run(ctx, job); err != nil {
		t.Fatalf("run interview: %v", err)
	}

	if n := countJobs(t, pool, JobSummarize, cs.ID); n != 1 {
		t.Errorf("работ саммари: %d, ожидалась 1", n)
	}
	if n := countJobs(t, pool, JobNotify, cs.ID); n != 0 {
		t.Errorf("исчерпанный пункт ушёл автору третьим вопросом: уведомлений %d", n)
	}
	if got := reload(t, cases, cs.ID).Gaps; len(got) != 2 {
		t.Errorf("пробелы не сохранены: %v", got)
	}
}

// fakeLLM подменяет транспорт клиента: ответ модели приходит из теста, сеть не
// нужна. Тест в том же пакете, поэтому обходится без параметра адреса.
func fakeLLM(t *testing.T, content string) *OpenRouter {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"content": content}}},
	})
	if err != nil {
		t.Fatalf("encode llm response: %v", err)
	}

	llm := NewOpenRouter("test-key", "test-model", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	llm.http = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})}
	return llm
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestAskedKeys: раунд задаёт несколько вопросов, а человек отвечает на один -
// остальные модель спрашивает снова. Счётчик по журналу решает, когда пункт
// исчерпан: третий заход по одному ключу автор читает как поломку бота.
func TestAskedKeys(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6009, 1)

	rounds := [][]Question{
		{{Key: "case", Text: "какой заказ?"}, {Key: "expected", Text: "что ожидали?"}},
		{{Key: "expected", Text: "а всё-таки, что должно было выйти?"}},
	}
	for n, questions := range rounds {
		err := addEvent(ctx, pool, cs.ID, "round_asked", map[string]any{
			"round": n + 1, "questions": questions,
		})
		if err != nil {
			t.Fatalf("add round: %v", err)
		}
	}

	asked, err := cases.askedKeys(ctx, cs.ID)
	if err != nil {
		t.Fatalf("asked keys: %v", err)
	}
	if asked["expected"] < maxAsks {
		t.Errorf("пункт expected спрошен %d раз, ожидалось не меньше %d", asked["expected"], maxAsks)
	}
	if asked["case"] >= maxAsks {
		t.Errorf("пункт case исчерпан после одного вопроса: %d", asked["case"])
	}
}

// TestReplaceJobRevivesPublish: работа обращения существует в единственном
// экземпляре, и новая заменяет прежнюю. Без этого исчерпавшая повторы
// публикация оставляла бы свой ключ в очереди, повторное «Публикую» молча не
// вставало бы, а обращение застряло бы в publishing навсегда.
func TestReplaceJobRevivesPublish(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6005, 3)

	if err := replaceJob(ctx, pool, JobPublish, cs.ID, casePayload{CaseID: cs.ID}); err != nil {
		t.Fatalf("put publish job: %v", err)
	}
	_, err := pool.Exec(ctx, `
		UPDATE jobs SET status = 'failed', attempts = 6 WHERE kind = $1`, JobPublish)
	if err != nil {
		t.Fatalf("fail publish job: %v", err)
	}

	_, err = pool.Exec(ctx, `UPDATE cases SET status = 'summary' WHERE id = $1`, cs.ID)
	if err != nil {
		t.Fatalf("move to summary: %v", err)
	}
	if err := cases.ConfirmSummary(ctx, reload(t, cases, cs.ID)); err != nil {
		t.Fatalf("confirm summary: %v", err)
	}

	var status string
	var attempts int
	err = pool.QueryRow(ctx, `
		SELECT status, attempts FROM jobs WHERE kind = $1 AND payload->>'case_id' = $2`,
		JobPublish, cs.ID).Scan(&status, &attempts)
	if err != nil {
		t.Fatalf("load publish job: %v", err)
	}
	if status != "pending" || attempts != 0 {
		t.Errorf("публикация не вернулась в очередь: status=%s attempts=%d", status, attempts)
	}
}

// TestAnswerAfterSummaryIsFix: правка саммари возвращает обращение в интервью, и
// следующий ход узнаёт в ней правку по журналу. Без этого автор, заметивший
// ошибку на последнем раунде, не может её исправить: предел раундов исчерпан, и
// правка молча ушла бы в новое саммари.
func TestAnswerAfterSummaryIsFix(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6002, 3)

	err := addEvent(ctx, pool, cs.ID, "round_asked", map[string]any{
		"round": 3, "questions": []Question{{Key: "case", Text: "какой заказ?"}},
	})
	if err != nil {
		t.Fatalf("add round: %v", err)
	}
	if err := addEvent(ctx, pool, cs.ID, "summary_ready", map[string]any{"incomplete": false}); err != nil {
		t.Fatalf("add summary event: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'summary' WHERE id = $1`, cs.ID); err != nil {
		t.Fatalf("move to summary: %v", err)
	}

	fix, err := cases.isFix(ctx, cs.ID)
	if err != nil {
		t.Fatalf("is fix: %v", err)
	}
	if !fix {
		t.Error("ход после показанного саммари не опознан как правка")
	}

	if err := cases.AddAnswer(ctx, reload(t, cases, cs.ID), "заказ был 4821, а не 4812"); err != nil {
		t.Fatalf("add answer: %v", err)
	}
	if got := reload(t, cases, cs.ID).Status; got != statusInterview {
		t.Errorf("статус после правки: %s, ожидался %s", got, statusInterview)
	}
	if n := countJobs(t, pool, JobInterview, cs.ID); n != 1 {
		t.Errorf("работ интервью после правки: %d, ожидалась 1", n)
	}
}

// TestShownSummaryIsInHistory: автор правит текст, который бот ему показал.
// Саммари обязано лежать в истории репликой бота, иначе правка ссылается в
// пустоту. Событие без снимка текста (обращение старше выката) пропускается.
func TestShownSummaryIsInHistory(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6012, 1)

	err := addEvent(ctx, pool, cs.ID, "round_asked", map[string]any{
		"round": 1, "questions": []Question{{Key: "case", Text: "какой заказ?"}},
	})
	if err != nil {
		t.Fatalf("add round: %v", err)
	}
	if err := addEvent(ctx, pool, cs.ID, "answer_given", map[string]any{"text": "заказ 4821"}); err != nil {
		t.Fatalf("add answer: %v", err)
	}
	err = addEvent(ctx, pool, cs.ID, "summary_ready", map[string]any{
		"title": "Заказ 4821 не уходит в доставку", "body": "## Конкретный случай\n\nЗаказ 4821.",
	})
	if err != nil {
		t.Fatalf("add summary: %v", err)
	}
	if err := addEvent(ctx, pool, cs.ID, "answer_given", map[string]any{"text": "заказ 4812"}); err != nil {
		t.Fatalf("add fix: %v", err)
	}

	history, err := cases.history(ctx, cs.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 4 {
		t.Fatalf("сообщений в истории: %d, ожидалось 4", len(history))
	}
	shown := history[2]
	if shown.Role != "assistant" {
		t.Errorf("роль показанного саммари: %s, ожидалась assistant", shown.Role)
	}
	if !strings.Contains(shown.Parts[0].text, "Заказ 4821 не уходит в доставку") {
		t.Errorf("в истории нет текста показанного саммари: %q", shown.Parts[0].text)
	}

	if err := addEvent(ctx, pool, cs.ID, "summary_ready", map[string]any{"incomplete": false}); err != nil {
		t.Fatalf("add legacy summary: %v", err)
	}
	history, err = cases.history(ctx, cs.ID)
	if err != nil {
		t.Fatalf("history after legacy: %v", err)
	}
	if len(history) != 4 {
		t.Errorf("событие без текста саммари попало в историю: %d сообщений", len(history))
	}
}

// TestStaleTurnIsDropped: пока модель думает, автор дописывает - и его ответ уже
// поставил свежий ход. Устаревший результат не имеет права лечь поверх: иначе
// автор получает два раунда вопросов на один свой ответ.
func TestStaleTurnIsDropped(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	i := newTestInterview(t, cases, 3)
	cs := startInterview(t, cases, 6006, 1)

	version, err := cases.turnsCount(ctx, pool, cs.ID)
	if err != nil {
		t.Fatalf("turns count: %v", err)
	}
	if err := cases.AddAnswer(ctx, cs, "и ещё вот что: падает только в Safari"); err != nil {
		t.Fatalf("add answer: %v", err)
	}

	turn := interviewTurn{
		Kind:      "bug",
		Filled:    []keyValue{{Key: "case", Value: "заказ 4821"}},
		Gaps:      []string{"expected", "actual"},
		Questions: []Question{{Key: "expected", Text: "что ожидали?"}},
	}
	saved, err := i.saveTurn(ctx, cs, turn, map[string]string{"case": "заказ 4821"}, 2, false, version)
	if err != nil {
		t.Fatalf("save turn: %v", err)
	}
	if saved {
		t.Error("ход, устаревший на ответе автора, записан в базу")
	}
	if got := reload(t, cases, cs.ID).Round; got != 1 {
		t.Errorf("раунд сдвинут устаревшим ходом: %d, ожидался 1", got)
	}
}

// TestRoundLimitGoesToSummary: исчерпав раунды, ход не спрашивает больше ничего,
// а собирает саммари с тем, что есть. Недобранный контракт даёт тикет с
// пробелами, а не отказ.
func TestRoundLimitGoesToSummary(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	i := newTestInterview(t, cases, 3)
	cs := startInterview(t, cases, 6007, 3)

	version, err := cases.turnsCount(ctx, pool, cs.ID)
	if err != nil {
		t.Fatalf("turns count: %v", err)
	}
	turn := interviewTurn{
		Kind:      "bug",
		Filled:    []keyValue{{Key: "case", Value: "заказ 4821"}},
		Gaps:      []string{"expected", "actual"},
		Questions: []Question{{Key: "expected", Text: "что ожидали?"}},
	}
	// Предел исчерпан (round=3 при пределе 3), поэтому ход идёт в саммари, а не
	// задаёт четвёртый раунд.
	saved, err := i.saveTurn(ctx, cs, turn, map[string]string{"case": "заказ 4821"}, 3, true, version)
	if err != nil {
		t.Fatalf("save turn: %v", err)
	}
	if !saved {
		t.Fatal("ход не записан")
	}

	if n := countJobs(t, pool, JobSummarize, cs.ID); n != 1 {
		t.Errorf("работ саммари: %d, ожидалась 1", n)
	}
	if n := countJobs(t, pool, JobNotify, cs.ID); n != 0 {
		t.Errorf("на пределе раундов автору ушли вопросы: уведомлений %d", n)
	}
	if got := reload(t, cases, cs.ID); len(got.Gaps) != 2 {
		t.Errorf("пробелы не сохранены: %v", got.Gaps)
	}
}

// TestPublishSkipsCancelled: отменённое обращение не уходит в GitHub, повторная
// работа не создаёт второго тикета. Оба выхода срабатывают до первого запроса,
// поэтому клиент здесь пустой.
func TestPublishSkipsCancelled(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	publisher := NewPublisher(cases, NewGitHub("", GitHubAPI, nil, log), testRules(t), log, 0)

	cs := startInterview(t, cases, 6003, 3)
	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'cancelled' WHERE id = $1`, cs.ID); err != nil {
		t.Fatalf("cancel case: %v", err)
	}

	job := Job{ID: 1, Kind: JobPublish, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := publisher.Run(ctx, job); err != nil {
		t.Fatalf("publish cancelled case: %v", err)
	}
	if got := reload(t, cases, cs.ID); got.IssueNumber != 0 {
		t.Errorf("отменённое обращение получило тикет #%d", got.IssueNumber)
	}

	_, err := pool.Exec(ctx, `
		UPDATE cases SET status = 'publishing', issue_number = 42, issue_url = 'u' WHERE id = $1`, cs.ID)
	if err != nil {
		t.Fatalf("mark published: %v", err)
	}
	if err := publisher.Run(ctx, job); err != nil {
		t.Fatalf("publish twice: %v", err)
	}
	if got := reload(t, cases, cs.ID).IssueNumber; got != 42 {
		t.Errorf("повтор работы переписал тикет: #%d", got)
	}
}

// TestRecoverStuck: обращение, потерявшее свою работу, двигаться нечем - из
// publishing нет даже отмены. Восстановление возвращает работу в очередь, а
// обращения, ждущие человека, не трогает.
func TestRecoverStuck(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	stuck := startInterview(t, cases, 6008, 3)
	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'publishing' WHERE id = $1`, stuck.ID); err != nil {
		t.Fatalf("move to publishing: %v", err)
	}
	waiting := startInterview(t, cases, 6009, 1)

	if err := cases.RecoverStuck(ctx); err != nil {
		t.Fatalf("recover stuck: %v", err)
	}

	if n := countJobs(t, pool, JobPublish, stuck.ID); n != 1 {
		t.Errorf("публикация не возвращена в очередь: работ %d, ожидалась 1", n)
	}
	// Обращение в интервью ждёт ответа автора: работы там нет и быть не должно.
	if n := countJobs(t, pool, JobInterview, waiting.ID); n != 0 {
		t.Errorf("восстановление тронуло ждущее обращение: работ %d", n)
	}
}

// TestPrefixStable: стабильный префикс обязан идти первым сообщением и не
// меняться от хода к ходу. Любая изменяющаяся строка перед промтом молча гасит
// кэш провайдера, и заметить это по ответам модели нельзя.
func TestPrefixStable(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	i := newTestInterview(t, cases, 3)
	cs := startInterview(t, cases, 6004, 1)

	first, err := i.dialog(ctx, cs, i.askPrefix)
	if err != nil {
		t.Fatalf("dialog: %v", err)
	}

	err = addEvent(ctx, pool, cs.ID, "round_asked", map[string]any{
		"round": 1, "questions": []Question{{Key: "case", Text: "какой заказ?"}},
	})
	if err != nil {
		t.Fatalf("add round: %v", err)
	}
	if err := cases.AddAnswer(ctx, reload(t, cases, cs.ID), "заказ 4821"); err != nil {
		t.Fatalf("add answer: %v", err)
	}

	second, err := i.dialog(ctx, reload(t, cases, cs.ID), i.askPrefix)
	if err != nil {
		t.Fatalf("dialog after round: %v", err)
	}

	if len(second) <= len(first) {
		t.Fatalf("история не выросла: было %d сообщений, стало %d", len(first), len(second))
	}
	if first[0].Role != "system" || second[0].Role != "system" {
		t.Fatal("первым сообщением обязан идти системный промт")
	}
	if first[0].Parts[0].text != second[0].Parts[0].text {
		t.Error("системный префикс изменился между ходами")
	}
}
