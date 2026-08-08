package app

import (
	"context"
	"encoding/json"
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
}

// TestAnswerAfterSummaryIsFix: правка саммари возвращает обращение в интервью и
// идёт признаком fix. Без него автор, заметивший ошибку на последнем раунде, не
// может её исправить: предел раундов уже исчерпан.
func TestAnswerAfterSummaryIsFix(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := startInterview(t, cases, 6002, 3)

	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'summary' WHERE id = $1`, cs.ID); err != nil {
		t.Fatalf("move to summary: %v", err)
	}

	if err := cases.AddAnswer(ctx, reload(t, cases, cs.ID), "заказ был 4821, а не 4812"); err != nil {
		t.Fatalf("add answer: %v", err)
	}
	if got := reload(t, cases, cs.ID).Status; got != statusInterview {
		t.Errorf("статус после правки: %s, ожидался %s", got, statusInterview)
	}

	var payload []byte
	err := pool.QueryRow(ctx, `
		SELECT payload FROM jobs WHERE kind = $1 AND payload->>'case_id' = $2`,
		JobInterview, cs.ID).Scan(&payload)
	if err != nil {
		t.Fatalf("load interview job: %v", err)
	}
	var p interviewPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("decode job payload: %v", err)
	}
	if !p.Fix {
		t.Errorf("работа правки без признака fix: %s", payload)
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
