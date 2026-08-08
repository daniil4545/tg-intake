package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
		name string
		turn interviewTurn
		ok   bool
	}{
		{
			name: "готовый ход",
			turn: interviewTurn{Kind: "bug", Filled: full, Ready: true},
			ok:   true,
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
			err := i.checkTurn(tt.turn)
			if tt.ok && err != nil {
				t.Errorf("ход отклонён: %v", err)
			}
			if !tt.ok && err == nil {
				t.Error("ход принят, ожидался отказ")
			}
		})
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

// TestRenderSections: порядок разделов задают правила, а не ответ модели, и
// заголовки берутся оттуда же. Одно место задаёт и что спрашиваем, и как это
// выглядит в тикете.
func TestRenderSections(t *testing.T) {
	i := newTestInterview(t, nil, 3)

	body := i.renderSections("bug", []section{
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
	publisher := NewPublisher(cases, NewGitHub("", log), testRules(t), log)

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
