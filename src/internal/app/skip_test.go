package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// skip_test.go - сценарии проверки §3b плана среза «Отправить как есть»
// (docs/plans/plan-skip-questions-3b.md). Реализации ещё нет: тесты пишутся по
// спеке и сигнатурам раздела 5 плана, поэтому красные до кода - это ожидаемо.

// skipFixture - обращение по умолчанию §3b: раунд 1 задан вопросом с догадкой
// по ядровому пункту "case", пункт ещё не закрыт, живой экран - сообщение 100.
func skipFixture(t *testing.T, cases *Cases, userID int64) *Case {
	t.Helper()
	ctx := context.Background()

	cs := startInterview(t, cases, userID, 1)
	if _, err := cases.pool.Exec(ctx,
		`UPDATE cases SET contract = '{"wrong":"статус не изменился"}', gaps = '["case"]' WHERE id = $1`,
		cs.ID); err != nil {
		t.Fatalf("set contract: %v", err)
	}
	questions := []Question{{Key: "case", Text: "какой заказ?", Suggested: "заказ 4821"}}
	if err := addEvent(ctx, cases.pool, cs.ID, "round_asked",
		map[string]any{"round": 1, "questions": questions}); err != nil {
		t.Fatalf("add round: %v", err)
	}
	if err := cases.SetScreen(ctx, cs.ID, 100, 1); err != nil {
		t.Fatalf("set screen: %v", err)
	}
	return reload(t, cases, cs.ID)
}

// skippedTurn - ход модели, который снова просит закрыть пункт "case": в
// сценариях после пропуска отсев обязан отбросить этот вопрос сам, ходу
// перепроверять предложение модели не нужно. Догадка непустая - иначе askTurn
// делает вторую попытку ради кнопки «Всё так», и стаб отвечает дважды.
const skippedTurn = `{"kind":"bug","filled":[],"gaps":["case"],"ready":false,` +
	`"questions":[{"key":"case","text":"какой заказ?","suggested":"заказ 4821"}]}`

// answeredCallbacks - число ответов на нажатие кнопки. Ревью среза 4: Telegram
// отвергает второй answerCallbackQuery на тот же callback_query_id, поэтому
// каждое нажатие обязано получить ровно один ответ, не ноль и не два.
func answeredCallbacks(ft *fakeTelegram) int {
	return len(ft.methodCalls("answerCallbackQuery"))
}

func countSkipEvents(t *testing.T, cases *Cases, caseID string) int {
	t.Helper()

	var n int
	err := cases.pool.QueryRow(context.Background(), `SELECT count(*) FROM case_events
		WHERE case_id = $1 AND kind = 'questions_skipped'`, caseID).Scan(&n)
	if err != nil {
		t.Fatalf("count skip events: %v", err)
	}
	return n
}

func countEventKind(t *testing.T, cases *Cases, caseID, kind string) int {
	t.Helper()

	var n int
	err := cases.pool.QueryRow(context.Background(), `SELECT count(*) FROM case_events
		WHERE case_id = $1 AND kind = $2`, caseID, kind).Scan(&n)
	if err != nil {
		t.Fatalf("count %s events: %v", kind, err)
	}
	return n
}

// capturingLLM - как fakeLLM, но дополнительно отдаёт последний отправленный
// запрос: Р-15 проверяет, что ответ автора, данный после пропуска, доходит до
// модели саммари, а не выбрасывается вместе с вопросами.
func capturingLLM(t *testing.T, content string) (*OpenRouter, *string) {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"content": content}}},
	})
	if err != nil {
		t.Fatalf("encode llm response: %v", err)
	}

	var last string
	llm := NewOpenRouter("test-key", "test-model", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	llm.http = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		last = string(raw)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})}
	return llm, &last
}

// Строка 1 §3b: пропуск не публикует сам по себе - работу довершает summarize,
// и пробел по пропущенному пункту виден автору строкой «Не уточнено» (Р-5).
func TestSkipQuestions(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := skipFixture(t, cases, 9000)

	if err := cases.SkipQuestions(ctx, cs, 1); err != nil {
		t.Fatalf("skip questions: %v", err)
	}

	if n := countSkipEvents(t, cases, cs.ID); n != 1 {
		t.Fatalf("событий пропуска: %d, ожидалась 1", n)
	}
	var payload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM case_events
		WHERE case_id = $1 AND kind = 'questions_skipped'`, cs.ID).Scan(&payload); err != nil {
		t.Fatalf("read skip event: %v", err)
	}
	var p struct {
		Round int `json:"round"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || p.Round != 1 {
		t.Errorf("payload события: %s, ожидался round=1", payload)
	}

	if n := countJobs(t, pool, JobSummarize, cs.ID); n != 1 {
		t.Errorf("работ саммари: %d, ожидалась 1", n)
	}
	if n := countJobs(t, pool, JobInterview, cs.ID); n != 0 {
		t.Errorf("работа интервью осталась после пропуска: %d", n)
	}
	if n := countJobs(t, pool, JobPublish, cs.ID); n != 0 {
		t.Errorf("работа публикации после пропуска: %d", n)
	}

	before := reload(t, cases, cs.ID)
	if before.Status != statusInterview || before.Round != 1 {
		t.Errorf("состояние сразу после пропуска: status=%s round=%d, саммари ещё не собрано",
			before.Status, before.Round)
	}

	i := newTestInterview(t, cases, 3)
	i.llm = fakeLLM(t, `{"title":"Форма не сохраняется","brief":"","sections":[]}`)
	job := Job{ID: 1, Kind: JobSummarize, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := i.Summarize(ctx, job); err != nil {
		t.Fatalf("summarize: %v", err)
	}

	after := reload(t, cases, cs.ID)
	if after.Status != statusSummary {
		t.Errorf("статус после саммари: %s, ожидался %s", after.Status, statusSummary)
	}

	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM jobs
		WHERE kind = $1 AND payload->>'case_id' = $2`, JobNotify, cs.ID).Scan(&raw); err != nil {
		t.Fatalf("notify job: %v", err)
	}
	var notify notifyPayload
	if err := json.Unmarshal(raw, &notify); err != nil {
		t.Fatalf("decode notify payload: %v", err)
	}
	if notify.Buttons != keysSummary {
		t.Errorf("кнопки уведомления: %q, ожидалось %q", notify.Buttons, keysSummary)
	}
	if !strings.Contains(notify.Text, "Не уточнено: конкретный случай.") {
		t.Errorf("нет пометки пробела по пропущенному вопросу:\n%s", notify.Text)
	}
}

// Строка 2 §3b: нажатие кнопки - toast «Принято», правка текста раунда без
// клавиатуры, событие в журнале.
func TestSkipButton(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	ft, tb := newFakeTelegram(t)
	log, _ := screenLog()
	b := screenBot(tb, pool, cases, log)
	cs := skipFixture(t, cases, 9001)

	if err := b.onSkip(callbackCtx(tb, cs.UserID, 100, "1")); err != nil {
		t.Fatalf("onSkip: %v", err)
	}

	if n := answeredCallbacks(ft); n != 1 {
		t.Fatalf("ответов на нажатие: %d, ожидался 1", n)
	}
	if got := lastToast(ft); got != "Принято" {
		t.Errorf("тост: %q, ожидалось «Принято»", got)
	}
	edits := ft.textEditsOf(100)
	if len(edits) != 1 {
		t.Fatalf("правок текста 100: %d, ожидалась 1", len(edits))
	}
	if _, ok := edits[0].body["reply_markup"]; ok {
		t.Errorf("правка раунда несёт клавиатуру: %v", edits[0].body)
	}
	text, _ := edits[0].body["text"].(string)
	if !strings.Contains(text, "Отправляю как есть") {
		t.Errorf("текст правки не называет пропуск: %q", text)
	}
	if n := countSkipEvents(t, cases, cs.ID); n != 1 {
		t.Errorf("событий пропуска: %d, ожидалась 1", n)
	}
}

// Строки 3-4 §3b: повтор той же кнопки и «Всё так» после пропуска - оба читают
// раунд закрытым и отвечают тем же тостом «устарел», не открывая второго
// события и не принимая скрытого ответа.
func TestSkipTwice(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	t.Run("повтор пропуска", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)
		cs := skipFixture(t, cases, 9002)

		if err := b.onSkip(callbackCtx(tb, cs.UserID, 100, "1")); err != nil {
			t.Fatalf("первое нажатие: %v", err)
		}
		if err := b.onSkip(callbackCtx(tb, cs.UserID, 100, "1")); err != nil {
			t.Fatalf("второе нажатие: %v", err)
		}

		if n := answeredCallbacks(ft); n != 2 {
			t.Fatalf("ответов на два нажатия: %d, ожидалось 2", n)
		}
		if got := lastToast(ft); got != "Этот экран устарел" {
			t.Errorf("тост повтора: %q", got)
		}
		if len(ft.stripsOf(100)) != 1 {
			t.Errorf("кнопка не снята после повтора: %v", ft.calls)
		}
		if n := countSkipEvents(t, cases, cs.ID); n != 1 {
			t.Errorf("событий пропуска после повтора: %d, ожидалась 1", n)
		}
		if n := countJobs(t, pool, JobSummarize, cs.ID); n != 1 {
			t.Errorf("работ саммари после повтора: %d, ожидалась 1", n)
		}
	})

	t.Run("«Всё так» после пропуска", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)
		cs := skipFixture(t, cases, 9003)

		if err := b.onSkip(callbackCtx(tb, cs.UserID, 100, "1")); err != nil {
			t.Fatalf("пропуск: %v", err)
		}
		ft.calls = nil // дальше проверяется ответ только на «Всё так»

		if err := b.onAllTrue(callbackCtx(tb, cs.UserID, 100, "1")); err != nil {
			t.Fatalf("всё так: %v", err)
		}
		if n := answeredCallbacks(ft); n != 1 {
			t.Fatalf("ответов на «Всё так»: %d, ожидался 1", n)
		}
		if got := lastToast(ft); got != "Этот экран устарел" {
			t.Errorf("тост «Всё так» после пропуска: %q", got)
		}
		if len(ft.stripsOf(100)) != 1 {
			t.Errorf("кнопка «Всё так» не снята: %v", ft.calls)
		}
		if n, err := cases.turnsCount(ctx, pool, cs.ID); err != nil || n != 0 {
			t.Errorf("answer_given после «Всё так» на пропущенном раунде: n=%d err=%v", n, err)
		}
	})
}

// Строки 5-6 §3b: ответ (текстом или «Всё так») закрывает раунд раньше
// пропуска - вторая кнопка приходит поздно и молчит, ход интервью на месте.
func TestSkipAfterAnswer(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	tests := []struct {
		name   string
		answer func(cs *Case) error
	}{
		{"после текстового ответа", func(cs *Case) error {
			return cases.AddAnswer(ctx, cs, "заказ 4821")
		}},
		{"после «Всё так»", func(cs *Case) error {
			return cases.AcceptRound(ctx, cs, 1)
		}},
	}
	for n, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ft, tb := newFakeTelegram(t)
			log, _ := screenLog()
			b := screenBot(tb, pool, cases, log)
			cs := skipFixture(t, cases, int64(9004+n))

			if err := tt.answer(cs); err != nil {
				t.Fatalf("ответ на раунд: %v", err)
			}

			if err := b.onSkip(callbackCtx(tb, cs.UserID, 100, "1")); err != nil {
				t.Fatalf("onSkip: %v", err)
			}

			if got := answeredCallbacks(ft); got != 1 {
				t.Fatalf("ответов на нажатие: %d, ожидался 1", got)
			}
			if got := lastToast(ft); got != "Этот экран устарел" {
				t.Errorf("тост: %q", got)
			}
			if n := countSkipEvents(t, cases, cs.ID); n != 0 {
				t.Errorf("событие пропуска после ответа: %d, ожидалось 0", n)
			}
			if got := countJobs(t, pool, JobInterview, cs.ID); got != 1 {
				t.Errorf("работ интервью: %d, ожидалась 1", got)
			}
			if got := countJobs(t, pool, JobSummarize, cs.ID); got != 0 {
				t.Errorf("работа саммари после ответа: %d, ожидалось 0", got)
			}
		})
	}
}

// Строки 7-10 §3b: отказ пропуска молчит - чужой экран, прошлый раунд, статус
// не interview и нечисловой раунд ведут к одному тосту без события и работы.
func TestSkipRefused(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	t.Run("не живой экран", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)
		cs := skipFixture(t, cases, 9006)

		if err := b.onSkip(callbackCtx(tb, cs.UserID, 99, "1")); err != nil {
			t.Fatalf("onSkip: %v", err)
		}
		if got := answeredCallbacks(ft); got != 1 {
			t.Fatalf("ответов на нажатие: %d, ожидался 1", got)
		}
		if got := lastToast(ft); got != "Этот экран устарел" {
			t.Errorf("тост: %q", got)
		}
		if len(ft.stripsOf(99)) != 1 {
			t.Errorf("нажатая кнопка 99 не снята: %v", ft.calls)
		}
		if n := countSkipEvents(t, cases, cs.ID); n != 0 {
			t.Errorf("событие пропуска с чужого экрана: %d", n)
		}
		if got := countJobs(t, pool, JobSummarize, cs.ID); got != 0 {
			t.Errorf("работа саммари с чужого экрана: %d", got)
		}
	})

	t.Run("раунд не текущий", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)
		cs := skipFixture(t, cases, 9007)
		if _, err := pool.Exec(ctx, `UPDATE cases SET round = 2 WHERE id = $1`, cs.ID); err != nil {
			t.Fatalf("bump round: %v", err)
		}
		if err := cases.ResetScreen(ctx, cs.ID, 100); err != nil {
			t.Fatalf("reset screen: %v", err)
		}

		if err := b.onSkip(callbackCtx(tb, cs.UserID, 100, "1")); err != nil {
			t.Fatalf("onSkip: %v", err)
		}
		if got := answeredCallbacks(ft); got != 1 {
			t.Fatalf("ответов на нажатие: %d, ожидался 1", got)
		}
		if got := lastToast(ft); got != "Этот экран устарел" {
			t.Errorf("тост: %q", got)
		}
		if n := countSkipEvents(t, cases, cs.ID); n != 0 {
			t.Errorf("событие пропуска на чужом раунде: %d", n)
		}
	})

	t.Run("статус не interview", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)
		cs := skipFixture(t, cases, 9008)
		if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'summary' WHERE id = $1`, cs.ID); err != nil {
			t.Fatalf("move to summary: %v", err)
		}
		if err := cases.ResetScreen(ctx, cs.ID, 100); err != nil {
			t.Fatalf("reset screen: %v", err)
		}

		if err := b.onSkip(callbackCtx(tb, cs.UserID, 100, "1")); err != nil {
			t.Fatalf("onSkip: %v", err)
		}
		if got := answeredCallbacks(ft); got != 1 {
			t.Fatalf("ответов на нажатие: %d, ожидался 1", got)
		}
		if got := lastToast(ft); got != "Этот экран устарел" {
			t.Errorf("тост: %q", got)
		}
		if got := reload(t, cases, cs.ID).Status; got != statusSummary {
			t.Errorf("статус задет отказом: %s", got)
		}
	})

	t.Run("номер раунда не число", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)
		cs := skipFixture(t, cases, 9009)

		if err := b.onSkip(callbackCtx(tb, cs.UserID, 100, "x")); err != nil {
			t.Fatalf("onSkip: %v", err)
		}
		if got := answeredCallbacks(ft); got != 1 {
			t.Fatalf("ответов на нажатие: %d, ожидался 1", got)
		}
		if got := lastToast(ft); got != "Этот экран устарел" {
			t.Errorf("тост: %q", got)
		}
		if n := countSkipEvents(t, cases, cs.ID); n != 0 {
			t.Errorf("событие пропуска при нечисловом раунде: %d", n)
		}
	})
}

// Строка 11 §3b (Р-15): ответ, данный после пропуска, не пропадает - ход
// интервью его видит, отсев душит вопрос модели, а не сам ответ, и саммари
// собирается с этим ответом внутри.
func TestAnswerAfterSkip(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := skipFixture(t, cases, 9010)

	if err := cases.SkipQuestions(ctx, cs, 1); err != nil {
		t.Fatalf("skip questions: %v", err)
	}
	if err := cases.AddAnswer(ctx, reload(t, cases, cs.ID), "текст ответа"); err != nil {
		t.Fatalf("add answer: %v", err)
	}

	i := newTestInterview(t, cases, 3)
	i.llm = fakeLLM(t, skippedTurn)
	job := Job{ID: 1, Kind: JobInterview, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := i.Run(ctx, job); err != nil {
		t.Fatalf("run interview: %v", err)
	}

	if n := countEventKind(t, cases, cs.ID, "round_asked"); n != 1 {
		t.Errorf("событий round_asked: %d, ожидалось 1 (второй раунд не открыт)", n)
	}
	if n := countEventKind(t, cases, cs.ID, "interview_done"); n != 1 {
		t.Errorf("interview_done: %d, ожидалась 1", n)
	}
	if n := countJobs(t, pool, JobNotify, cs.ID); n != 0 {
		t.Errorf("уведомление раунда после пропуска: %d, ожидалось 0", n)
	}
	if n := countJobs(t, pool, JobSummarize, cs.ID); n != 1 {
		t.Errorf("работ саммари: %d, ожидалась 1", n)
	}

	captured, last := capturingLLM(t, `{"title":"Т","brief":"","sections":[]}`)
	i.llm = captured
	sumJob := Job{ID: 2, Kind: JobSummarize, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
	if err := i.Summarize(ctx, sumJob); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if !strings.Contains(*last, "текст ответа") {
		t.Errorf("запрос саммари не содержит ответ автора, данный после пропуска:\n%s", *last)
	}
}

// Строки 12-13 §3b: правка после показанного саммари наследует пропуск того же
// обращения (Р-15) и не открывает раунда; отсев не течёт в другое обращение.
func TestFixAfterSkip(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	runFix := func(t *testing.T, cs *Case) {
		i := newTestInterview(t, cases, 3)
		i.llm = fakeLLM(t, `{"title":"Т","brief":"","sections":[]}`)
		sumJob := Job{ID: 1, Kind: JobSummarize, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
		if err := i.Summarize(ctx, sumJob); err != nil {
			t.Fatalf("summarize: %v", err)
		}
		if got := reload(t, cases, cs.ID).Status; got != statusSummary {
			t.Fatalf("статус до правки: %s, ожидался %s", got, statusSummary)
		}

		if err := cases.AddAnswer(ctx, reload(t, cases, cs.ID), "правка"); err != nil {
			t.Fatalf("add answer: %v", err)
		}
		i.llm = fakeLLM(t, skippedTurn)
		job := Job{ID: 2, Kind: JobInterview, Payload: []byte(`{"case_id":"` + cs.ID + `"}`)}
		if err := i.Run(ctx, job); err != nil {
			t.Fatalf("run interview: %v", err)
		}
	}

	t.Run("правка после пропуска", func(t *testing.T) {
		cs := skipFixture(t, cases, 9011)
		if err := cases.SkipQuestions(ctx, cs, 1); err != nil {
			t.Fatalf("skip questions: %v", err)
		}
		runFix(t, cs)

		if n := countEventKind(t, cases, cs.ID, "round_asked"); n != 1 {
			t.Errorf("событий round_asked: %d, ожидалось 1 (правка не открыла раунд)", n)
		}
		if n := countEventKind(t, cases, cs.ID, "interview_done"); n != 1 {
			t.Errorf("interview_done: %d, ожидалась 1", n)
		}
		if n := countJobs(t, pool, JobSummarize, cs.ID); n != 1 {
			t.Errorf("работ саммари после правки: %d, ожидалась 1", n)
		}
		if got := reload(t, cases, cs.ID).Round; got != 1 {
			t.Errorf("cases.round после правки: %d, ожидался прежний 1", got)
		}
	})

	t.Run("второе обращение без пропуска", func(t *testing.T) {
		cs := skipFixture(t, cases, 9012)
		runFix(t, cs)

		if n := countEventKind(t, cases, cs.ID, "round_asked"); n != 2 {
			t.Errorf("событий round_asked без пропуска: %d, ожидалось 2 (раунд 2 задан)", n)
		}
	})
}

// Строка 13a §3b: пропуск, случившийся уже после того, как Run прочитал
// состояние и решил вопрос (round вычислен как cs.Round+1, toSummary=false),
// но до того, как saveTurn это состояние записал - гонка "вторая рука жмёт
// «Отправить как есть», пока первый ход ещё не сохранил свой". saveTurn
// обязан увидеть пропуск в своей же транзакции и не открыть второй раунд
// поверх уже закрытого, а записать прежний round.
func TestSkipDuringTurn(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := skipFixture(t, cases, 9013)

	if err := cases.SkipQuestions(ctx, reload(t, cases, cs.ID), 1); err != nil {
		t.Fatalf("skip during turn: %v", err)
	}

	i := newTestInterview(t, cases, 3)
	turn := interviewTurn{Kind: "bug", Gaps: []string{"case"}, Ready: false,
		Questions: []Question{{Key: "case", Text: "какой заказ?", Suggested: "заказ 4821"}}}
	saved, actualRound, actualToSummary, err := i.saveTurn(ctx, cs, turn, map[string]string{}, cs.Round+1, false, 0)
	if err != nil {
		t.Fatalf("save turn: %v", err)
	}
	if !saved {
		t.Fatalf("saveTurn счёл ход устаревшим, хотя версия разговора не менялась")
	}
	if actualRound != 1 || !actualToSummary {
		t.Errorf("решение saveTurn: round=%d toSummary=%v, ожидалось round=1 toSummary=true", actualRound, actualToSummary)
	}

	if n := countEventKind(t, cases, cs.ID, "round_asked"); n != 1 {
		t.Errorf("событий round_asked: %d, ожидалось 1 (второй раунд не открыт)", n)
	}
	if n := countEventKind(t, cases, cs.ID, "interview_done"); n != 1 {
		t.Errorf("interview_done: %d, ожидалась 1", n)
	}
	if n := countJobs(t, pool, JobSummarize, cs.ID); n != 1 {
		t.Errorf("работ саммари: %d, ожидалась 1", n)
	}
	if got := reload(t, cases, cs.ID).Round; got != 1 {
		t.Errorf("cases.round после гонки: %d, ожидался прежний 1", got)
	}
}

// Ревью среза 5, п.3: неизвестная ошибка AcceptRound (не одна из четырёх
// именованных) не должна получать тост - хендлер уходит с ней наверх без
// ответа на callback, второй раз (и только один) отвечает OnError (bot.go).
// Порченый payload раунда - валидный JSON, но не той формы: даёт lastQuestions
// внутри AcceptRound ошибку разбора, а не одну из четырёх именованных.
func TestAllTrueUnknownErrorNoToast(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	ft, tb := newFakeTelegram(t)
	log, _ := screenLog()
	b := screenBot(tb, pool, cases, log)
	cs := skipFixture(t, cases, 9014)

	if _, err := pool.Exec(ctx, `UPDATE case_events SET payload = '{"round":1,"questions":"oops"}'
		WHERE case_id = $1 AND kind = 'round_asked'`, cs.ID); err != nil {
		t.Fatalf("corrupt round payload: %v", err)
	}

	if err := b.onAllTrue(callbackCtx(tb, cs.UserID, 100, "1")); err == nil {
		t.Fatalf("onAllTrue не вернул ошибку разбора вопросов")
	}
	if n := answeredCallbacks(ft); n != 0 {
		t.Errorf("хендлер сам ответил на неизвестную ошибку: %d - в проде дубль с OnError", n)
	}
}

// Строка 14 §3b: клавиатура раунда несёт пропуск всегда, «Всё так» - только
// когда есть что подтверждать; обе кнопки укладываются в лимит Telegram на
// callback_data.
func TestRoundKeyboardSkip(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 8900, 2)

	tests := []struct {
		name    string
		buttons string
		want    map[string]string // unique -> текст кнопки
	}{
		{"keysRound - есть догадка", keysRound,
			map[string]string{"all_true": "Всё так", "skip": "Отправить как есть"}},
		{"keysAsk - без догадки", keysAsk,
			map[string]string{"skip": "Отправить как есть"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ft, tb := newFakeTelegram(t)
			log, _ := screenLog()
			b := screenBot(tb, pool, cases, log)

			if err := b.Notify(ctx, notifyRoundJob(t, cs.ID, cs.Round, "Раунд вопросов", tt.buttons)); err != nil {
				t.Fatalf("notify: %v", err)
			}
			sends := ft.methodCalls("sendMessage")
			if len(sends) != 1 {
				t.Fatalf("сообщений: %d, ожидалось 1", len(sends))
			}
			rows := inlineRows(t, sends[0])
			if len(rows) != 1 {
				t.Fatalf("строк клавиатуры: %d, ожидалась 1", len(rows))
			}
			if len(rows[0]) != len(tt.want) {
				t.Fatalf("кнопок в строке: %d, ожидалось %d: %+v", len(rows[0]), len(tt.want), rows[0])
			}
			seen := map[string]bool{}
			for _, btn := range rows[0] {
				wantText, ok := tt.want[btn.Unique]
				if !ok {
					t.Errorf("неожиданная кнопка %q", btn.Unique)
					continue
				}
				seen[btn.Unique] = true
				if btn.Text != wantText {
					t.Errorf("кнопка %s: текст %q, ожидалось %q", btn.Unique, btn.Text, wantText)
				}
				if !strings.Contains(btn.Data, "|2") {
					t.Errorf("кнопка %s: callback_data %q не несёт номер раунда 2", btn.Unique, btn.Data)
				}
				if len(btn.Data) > 64 {
					t.Errorf("кнопка %s: callback_data длиннее 64 байт: %d", btn.Unique, len(btn.Data))
				}
			}
			for unique := range tt.want {
				if !seen[unique] {
					t.Errorf("кнопка %q отсутствует в клавиатуре", unique)
				}
			}
		})
	}
}
