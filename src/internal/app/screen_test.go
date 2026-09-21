package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tele "gopkg.in/telebot.v4"
)

// screen_test.go - сценарии проверки §3b плана среза «живой экран»
// (docs/plans/plan-live-screen-3b.md). Реализации ещё нет: тесты пишутся по
// спеке и сигнатурам раздела 5 плана, поэтому красные до кода - это ожидаемо.

// notifyJob - работа notify с заданным текстом и набором кнопок, как её кладёт
// case.go. ID произвольный: HandleFailedJob этими тестами не проверяется.
func notifyJob(t *testing.T, caseID, text, buttons string) Job {
	t.Helper()
	raw, err := json.Marshal(notifyPayload{CaseID: caseID, Text: text, Buttons: buttons})
	if err != nil {
		t.Fatalf("marshal notify payload: %v", err)
	}
	return Job{ID: 1, Kind: JobNotify, Payload: raw}
}

// notifyRoundJob - то же, но с номером раунда: keysRound/keysAsk несут его в
// payload, чтобы Notify отличал запоздавшую доставку от текущего раунда.
func notifyRoundJob(t *testing.T, caseID string, round int, text, buttons string) Job {
	t.Helper()
	raw, err := json.Marshal(notifyPayload{CaseID: caseID, Text: text, Buttons: buttons, Round: round})
	if err != nil {
		t.Fatalf("marshal notify payload: %v", err)
	}
	return Job{ID: 1, Kind: JobNotify, Payload: raw}
}

// lastToast - текст последнего answerCallbackQuery: то, что видит автор
// всплывающей подсказкой на нажатие.
func lastToast(ft *fakeTelegram) string {
	calls := ft.methodCalls("answerCallbackQuery")
	if len(calls) == 0 {
		return ""
	}
	text, _ := calls[len(calls)-1].body["text"].(string)
	return text
}

func caseUpdatedAt(t *testing.T, pool *pgxpool.Pool, caseID string) time.Time {
	t.Helper()
	var ts time.Time
	if err := pool.QueryRow(context.Background(), `SELECT updated_at FROM cases WHERE id = $1`, caseID).
		Scan(&ts); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	return ts
}

// brokenPool - пул, у которого любой запрос отказывает немедленно: имитирует
// «Active вернул ошибку» без сети и без гонок.
func brokenPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	pool.Close()
	return pool
}

// Строка 2 §3b: SetScreen пишет только свои колонки, и рестарт видит их через
// новый Cases на том же пуле, а не через память процесса.
func TestScreenSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	cs, _, err := cases.StartCase(ctx, User{ID: 8000, First: "Тест"}, "tg-intake", modeTicket)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	before := caseUpdatedAt(t, pool, cs.ID)

	if err := cases.SetScreen(ctx, cs.ID, 777, 2); err != nil {
		t.Fatalf("set screen: %v", err)
	}

	fresh := newTestCases(t, pool, t.TempDir())
	reloaded, err := fresh.Load(ctx, cs.ID)
	if err != nil || reloaded == nil {
		t.Fatalf("load after restart: %v", err)
	}
	if reloaded.Screen != 777 || reloaded.ScreenRound != 2 {
		t.Errorf("экран после рестарта: msg=%d round=%d, ожидалось 777 и 2",
			reloaded.Screen, reloaded.ScreenRound)
	}
	if after := caseUpdatedAt(t, pool, cs.ID); !after.Equal(before) {
		t.Errorf("updated_at сдвинулся записью экрана: было %v, стало %v", before, after)
	}
}

// Строки 3-4 §3b: кнопка не с живого экрана толкует как устаревшую - тост,
// снятие нажатой кнопки, ни работы, ни ответа; та же кнопка с id живого
// экрана проходит как обычно. Проверено на всех трёх кнопках шага.
func TestStaleStepButton(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	t.Run("публикую", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 8101, 1)
		if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'summary' WHERE id = $1`, cs.ID); err != nil {
			t.Fatalf("move to summary: %v", err)
		}
		if err := cases.SetScreen(ctx, cs.ID, 100, 0); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.onPublish(callbackCtx(tb, cs.UserID, 99, "")); err != nil {
			t.Fatalf("onPublish (99): %v", err)
		}
		if got := lastToast(ft); got != "Этот экран устарел" {
			t.Errorf("toast: %q, ожидалось «Этот экран устарел»", got)
		}
		if len(ft.stripsOf(99)) != 1 {
			t.Errorf("кнопка 99 не снята: %v", ft.calls)
		}
		if n := countJobs(t, pool, JobPublish, cs.ID); n != 0 {
			t.Errorf("работа publish поставлена по чужому экрану: %d", n)
		}
		if got := reload(t, cases, cs.ID).Status; got != statusSummary {
			t.Errorf("статус: %s, ожидался %s", got, statusSummary)
		}

		if err := b.onPublish(callbackCtx(tb, cs.UserID, 100, "")); err != nil {
			t.Fatalf("onPublish (100): %v", err)
		}
		if got := lastToast(ft); got != "Публикую" {
			t.Errorf("toast с живого экрана: %q, ожидалось «Публикую»", got)
		}
		if n := countJobs(t, pool, JobPublish, cs.ID); n != 1 {
			t.Errorf("работ publish с живого экрана: %d, ожидалась 1", n)
		}
	})

	t.Run("всё так", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 8102, 1)
		questions := []Question{{Key: "case", Text: "какой заказ?", Suggested: "заказ 4821"}}
		if err := addEvent(ctx, pool, cs.ID, "round_asked",
			map[string]any{"round": 1, "questions": questions}); err != nil {
			t.Fatalf("add round: %v", err)
		}
		if err := cases.SetScreen(ctx, cs.ID, 100, 1); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.onAllTrue(callbackCtx(tb, cs.UserID, 99, "1")); err != nil {
			t.Fatalf("onAllTrue (99): %v", err)
		}
		if got := lastToast(ft); got != "Этот экран устарел" {
			t.Errorf("toast: %q, ожидалось «Этот экран устарел»", got)
		}
		if len(ft.stripsOf(99)) != 1 {
			t.Errorf("кнопка 99 не снята: %v", ft.calls)
		}
		if n, err := cases.turnsCount(ctx, pool, cs.ID); err != nil || n != 0 {
			t.Errorf("answer_given по чужому экрану: n=%d err=%v", n, err)
		}

		if err := b.onAllTrue(callbackCtx(tb, cs.UserID, 100, "1")); err != nil {
			t.Fatalf("onAllTrue (100): %v", err)
		}
		if n, err := cases.turnsCount(ctx, pool, cs.ID); err != nil || n != 1 {
			t.Errorf("answer_given с живого экрана: n=%d err=%v", n, err)
		}
	})

	t.Run("поправить", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 8103, 2)
		if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'summary' WHERE id = $1`, cs.ID); err != nil {
			t.Fatalf("move to summary: %v", err)
		}
		if err := cases.SetScreen(ctx, cs.ID, 100, 0); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.onFix(callbackCtx(tb, cs.UserID, 99, "")); err != nil {
			t.Fatalf("onFix (99): %v", err)
		}
		if got := lastToast(ft); got != "Этот экран устарел" {
			t.Errorf("toast: %q, ожидалось «Этот экран устарел»", got)
		}
		if len(ft.stripsOf(99)) != 1 {
			t.Errorf("кнопка 99 не снята: %v", ft.calls)
		}
		if len(ft.textEditsOf(100)) != 0 {
			t.Errorf("экран 100 задет чужой кнопкой: %v", ft.textEditsOf(100))
		}

		if err := b.onFix(callbackCtx(tb, cs.UserID, 100, "")); err != nil {
			t.Fatalf("onFix (100): %v", err)
		}
		if got := lastToast(ft); got != "Жду правку" {
			t.Errorf("toast с живого экрана: %q, ожидалось «Жду правку»", got)
		}
		if len(ft.textEditsOf(100)) != 1 {
			t.Errorf("экран 100 не переписан: %v", ft.textEditsOf(100))
		}
	})
}

// Строка 5 §3b: screen_msg = 0 значит «экрана нет», кнопка проходит без
// сверки id.
func TestStepWithoutScreen(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	ft, tb := newFakeTelegram(t)
	log, _ := screenLog()
	b := screenBot(tb, pool, cases, log)

	cs := startInterview(t, cases, 8200, 1)
	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'summary' WHERE id = $1`, cs.ID); err != nil {
		t.Fatalf("move to summary: %v", err)
	}

	if err := b.onPublish(callbackCtx(tb, cs.UserID, 99, "")); err != nil {
		t.Fatalf("onPublish: %v", err)
	}
	if got := lastToast(ft); got != "Публикую" {
		t.Errorf("toast: %q, ожидалось «Публикую»", got)
	}
	if n := countJobs(t, pool, JobPublish, cs.ID); n != 1 {
		t.Errorf("работ publish: %d, ожидалась 1", n)
	}
}

// Строка 6 §3b (S2): второе «Публикую» на живом экране - «Уже публикую», а не
// «экран устарел»: работа одна по ключу.
func TestPublishTwice(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	ft, tb := newFakeTelegram(t)
	log, _ := screenLog()
	b := screenBot(tb, pool, cases, log)

	cs := startInterview(t, cases, 8300, 1)
	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'summary' WHERE id = $1`, cs.ID); err != nil {
		t.Fatalf("move to summary: %v", err)
	}
	if err := cases.SetScreen(ctx, cs.ID, 100, 0); err != nil {
		t.Fatalf("set screen: %v", err)
	}

	if err := b.onPublish(callbackCtx(tb, cs.UserID, 100, "")); err != nil {
		t.Fatalf("onPublish 1: %v", err)
	}
	if n := countJobs(t, pool, JobPublish, cs.ID); n != 1 {
		t.Fatalf("работ publish после первого нажатия: %d, ожидалась 1", n)
	}

	if err := b.onPublish(callbackCtx(tb, cs.UserID, 100, "")); err != nil {
		t.Fatalf("onPublish 2: %v", err)
	}
	if got := lastToast(ft); got != "Публикую" {
		t.Errorf("toast второго нажатия: %q, экран не должен читаться устаревшим", got)
	}
	if n := countJobs(t, pool, JobPublish, cs.ID); n != 1 {
		t.Errorf("повтор поставил вторую работу: %d, ожидалась 1", n)
	}
	sends := ft.methodCalls("sendMessage")
	if len(sends) == 0 || !strings.Contains(fmt.Sprint(sends[len(sends)-1].body["text"]), "Уже публикую") {
		t.Errorf("ответ на повтор: %v, ожидалось «Уже публикую»", sends)
	}
}

// Строка 7 §3b (R8): шаг снимает прежний живой экран раньше отправки нового
// и переносит на него раунд обращения.
func TestStepStripsPrevious(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	ft, tb := newFakeTelegram(t)
	log, _ := screenLog()
	b := screenBot(tb, pool, cases, log)

	cs := startInterview(t, cases, 8400, 2)
	if err := cases.SetScreen(ctx, cs.ID, 100, 1); err != nil {
		t.Fatalf("set screen: %v", err)
	}

	if err := b.Notify(ctx, notifyRoundJob(t, cs.ID, cs.Round, "Раунд вопросов", keysRound)); err != nil {
		t.Fatalf("notify: %v", err)
	}

	stripIdx, sendIdx := ft.indexOf("editMessageReplyMarkup"), ft.indexOf("sendMessage")
	if stripIdx < 0 || sendIdx < 0 || stripIdx > sendIdx {
		t.Fatalf("порядок вызовов: снятие %d, отправка %d - ожидалось снятие раньше", stripIdx, sendIdx)
	}
	if len(ft.stripsOf(100)) != 1 {
		t.Errorf("прежний экран 100 не снят: %v", ft.calls)
	}

	fresh := reload(t, cases, cs.ID)
	if fresh.Screen == 0 || fresh.Screen == 100 {
		t.Errorf("screen_msg после шага: %d, ожидался новый id", fresh.Screen)
	}
	if fresh.ScreenRound != fresh.Round {
		t.Errorf("screen_round: %d, ожидался round обращения %d", fresh.ScreenRound, fresh.Round)
	}
}

// Строки 8-9 §3b: отказ снятия прежнего экрана не должен стопорить шаг -
// сообщение уходит и screen_msg обновляется в любом случае; в лог попадает
// только настоящий отказ, а не «уже не изменено».
func TestStripFailureKeepsStep(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	t.Run("нельзя редактировать", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, buf := screenLog()
		ft.fail = func(c tgCall) *tele.Error {
			if c.method == "editMessageReplyMarkup" && fmt.Sprint(c.body["message_id"]) == "100" {
				return tele.ErrCantEditMessage
			}
			return nil
		}
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 8500, 1)
		if err := cases.SetScreen(ctx, cs.ID, 100, 1); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.Notify(ctx, notifyRoundJob(t, cs.ID, cs.Round, "Раунд вопросов", keysRound)); err != nil {
			t.Fatalf("notify: %v", err)
		}
		if n := len(ft.methodCalls("sendMessage")); n != 1 {
			t.Fatalf("шаг не отправлен при отказе снятия: сообщений %d", n)
		}
		if fresh := reload(t, cases, cs.ID); fresh.Screen == 0 || fresh.Screen == 100 {
			t.Errorf("screen_msg после отказа снятия: %d, ожидался новый id", fresh.Screen)
		}
		if !strings.Contains(buf.String(), "screen_strip_failed") {
			t.Error("отказ снятия не залогирован")
		}
	})

	t.Run("уже не изменено", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, buf := screenLog()
		ft.fail = func(c tgCall) *tele.Error {
			if c.method == "editMessageReplyMarkup" && fmt.Sprint(c.body["message_id"]) == "100" {
				return tele.ErrMessageNotModified
			}
			return nil
		}
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 8501, 1)
		if err := cases.SetScreen(ctx, cs.ID, 100, 1); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.Notify(ctx, notifyRoundJob(t, cs.ID, cs.Round, "Раунд вопросов", keysRound)); err != nil {
			t.Fatalf("notify: %v", err)
		}
		if n := len(ft.methodCalls("sendMessage")); n != 1 {
			t.Fatalf("шаг не отправлен: сообщений %d", n)
		}
		if strings.Contains(buf.String(), "screen_strip_failed") {
			t.Error("«уже не изменено» залогировано как отказ")
		}
	})
}

// Ревью среза 4, п.2: Notify раунда, доставленный с опозданием (после того как
// разговор уже прошёл дальше), шагом не становится - обычное сообщение без
// кнопки, живой экран чужого прогресса не трогает.
func TestStaleRoundNotify(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	t.Run("раунд после следующего раунда", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 8850, 2)
		if err := cases.SetScreen(ctx, cs.ID, 200, 2); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.Notify(ctx, notifyRoundJob(t, cs.ID, 1, "Раунд вопросов 1", keysRound)); err != nil {
			t.Fatalf("notify: %v", err)
		}

		if len(ft.stripsOf(200)) != 0 {
			t.Errorf("живой экран раунда 2 снят запоздавшим раундом 1: %v", ft.calls)
		}
		if fresh := reload(t, cases, cs.ID); fresh.Screen != 200 || fresh.ScreenRound != 2 {
			t.Errorf("screen_msg/screen_round задеты: %d/%d, ожидалось 200/2", fresh.Screen, fresh.ScreenRound)
		}
		sends := ft.methodCalls("sendMessage")
		if len(sends) != 1 {
			t.Fatalf("запоздавший раунд не дошёл: сообщений %d", len(sends))
		}
		if _, ok := sends[0].body["reply_markup"]; ok {
			t.Errorf("запоздавший раунд пришёл с кнопкой шага: %v", sends[0].body)
		}
	})

	t.Run("раунд после саммари", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 8851, 1)
		if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'summary' WHERE id = $1`, cs.ID); err != nil {
			t.Fatalf("move to summary: %v", err)
		}
		if err := cases.SetScreen(ctx, cs.ID, 300, 0); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.Notify(ctx, notifyRoundJob(t, cs.ID, 1, "Раунд вопросов 1", keysRound)); err != nil {
			t.Fatalf("notify: %v", err)
		}

		if len(ft.stripsOf(300)) != 0 {
			t.Errorf("экран саммари («Публикую/Поправить») снят запоздавшим раундом: %v", ft.calls)
		}
		if got := reload(t, cases, cs.ID).Screen; got != 300 {
			t.Errorf("screen_msg: %d, ожидался прежний 300", got)
		}
	})
}

// Строки 10-11 §3b (R8): пометка раунда живёт в БД, а не в памяти процесса -
// новый Bot без единого прежнего вызова размечает и текстовый ответ, и
// «Всё так» на тот же экран.
func TestRoundMarkAfterRestart(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	questions := []Question{{Key: "case", Text: "какой заказ?", Suggested: "неважно"}}

	t.Run("два текста автора", func(t *testing.T) {
		cs := startInterview(t, cases, 8600, 1)
		if err := addEvent(ctx, pool, cs.ID, "round_asked",
			map[string]any{"round": 1, "questions": questions}); err != nil {
			t.Fatalf("add round: %v", err)
		}
		if err := cases.SetScreen(ctx, cs.ID, 100, 1); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		if err := b.onAnswer(ctx, textCtx(tb, cs.UserID, "заказ 4821, статус завис"),
			reload(t, cases, cs.ID)); err != nil {
			t.Fatalf("onAnswer 1: %v", err)
		}
		edits := ft.textEditsOf(100)
		if len(edits) != 1 || !strings.Contains(fmt.Sprint(edits[0].body["text"]), "Ответ принят") {
			t.Fatalf("правка после первого ответа: %v", edits)
		}

		if err := b.onAnswer(ctx, textCtx(tb, cs.UserID, "ещё уточнение"),
			reload(t, cases, cs.ID)); err != nil {
			t.Fatalf("onAnswer 2: %v", err)
		}
		edits = ft.textEditsOf(100)
		if len(edits) != 2 || !strings.Contains(fmt.Sprint(edits[1].body["text"]), "Принято ответов: 2") {
			t.Fatalf("правка после второго ответа: %v", edits)
		}
	})

	t.Run("всё так", func(t *testing.T) {
		cs := startInterview(t, cases, 8601, 1)
		if err := addEvent(ctx, pool, cs.ID, "round_asked",
			map[string]any{"round": 1, "questions": questions}); err != nil {
			t.Fatalf("add round: %v", err)
		}
		if err := cases.SetScreen(ctx, cs.ID, 100, 1); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		if err := b.onAllTrue(callbackCtx(tb, cs.UserID, 100, "1")); err != nil {
			t.Fatalf("onAllTrue: %v", err)
		}
		if n, err := cases.turnsCount(ctx, pool, cs.ID); err != nil || n != 1 {
			t.Errorf("answer_given после «Всё так»: n=%d err=%v", n, err)
		}
		if len(ft.textEditsOf(100)) == 0 {
			t.Error("сообщение раунда не тронуто")
		}
	})
}

// Строка 12 §3b (M1): «Поправить» переписывает экран саммари один раз, а
// текст самой правки - раунд не на экране саммари - идёт новым сообщением.
func TestFixTextKeepsSummary(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	ft, tb := newFakeTelegram(t)
	log, _ := screenLog()
	b := screenBot(tb, pool, cases, log)

	cs := startInterview(t, cases, 8700, 2)
	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'summary' WHERE id = $1`, cs.ID); err != nil {
		t.Fatalf("move to summary: %v", err)
	}
	if err := cases.SetScreen(ctx, cs.ID, 100, 0); err != nil {
		t.Fatalf("set screen: %v", err)
	}

	if err := b.onFix(callbackCtx(tb, cs.UserID, 100, "")); err != nil {
		t.Fatalf("onFix: %v", err)
	}
	if len(ft.textEditsOf(100)) != 1 {
		t.Fatalf("правка экрана саммари от «Поправить»: %v", ft.textEditsOf(100))
	}

	for _, text := range []string{"на самом деле заказ другой", "и ещё уточнение"} {
		if err := b.onAnswer(ctx, textCtx(tb, cs.UserID, text), reload(t, cases, cs.ID)); err != nil {
			t.Fatalf("onAnswer %q: %v", text, err)
		}
	}

	if n := len(ft.textEditsOf(100)); n != 1 {
		t.Errorf("тексты правки задели экран саммари: правок 100 стало %d", n)
	}
	if n := len(ft.methodCalls("sendMessage")); n != 2 {
		t.Errorf("ответы на правку новым сообщением: сообщений %d, ожидалось 2", n)
	}
}

// Строки 13-14 §3b (M1, S7): пометка не садится на чужой раунд и не садится,
// если текст с пометкой не помещается в одно сообщение - в обоих случаях
// ответ уходит новым сообщением, а не правкой экрана.
func TestMarkSkipsOtherRound(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	t.Run("экран прошлого раунда", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 8800, 2)
		questions := []Question{{Key: "case", Text: "какой заказ?", Suggested: "неважно"}}
		if err := addEvent(ctx, pool, cs.ID, "round_asked",
			map[string]any{"round": 2, "questions": questions}); err != nil {
			t.Fatalf("add round: %v", err)
		}
		// Notify по раунду 2 ещё не доставлен: на экране остался раунд 1.
		if err := cases.SetScreen(ctx, cs.ID, 100, 1); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.onAnswer(ctx, textCtx(tb, cs.UserID, "ответ на раунд"),
			reload(t, cases, cs.ID)); err != nil {
			t.Fatalf("onAnswer: %v", err)
		}
		if edits := ft.textEditsOf(100); len(edits) != 0 {
			t.Errorf("правка чужого раунда: %v", edits)
		}
		if n := len(ft.methodCalls("sendMessage")); n != 1 {
			t.Errorf("ответ новым сообщением: сообщений %d, ожидалось 1", n)
		}
	})

	t.Run("текст с пометкой длиннее предела", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 8801, 1)
		questions := []Question{{Key: "case", Text: strings.Repeat("а", maxMessage), Suggested: "неважно"}}
		if err := addEvent(ctx, pool, cs.ID, "round_asked",
			map[string]any{"round": 1, "questions": questions}); err != nil {
			t.Fatalf("add round: %v", err)
		}
		// Раунд на экране совпадает - причина отказа только в длине.
		if err := cases.SetScreen(ctx, cs.ID, 100, 1); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.onAnswer(ctx, textCtx(tb, cs.UserID, "ответ"),
			reload(t, cases, cs.ID)); err != nil {
			t.Fatalf("onAnswer: %v", err)
		}
		if edits := ft.textEditsOf(100); len(edits) != 0 {
			t.Errorf("правка длинного текста: %v", edits)
		}
		if n := len(ft.methodCalls("sendMessage")); n != 1 {
			t.Errorf("ответ новым сообщением: сообщений %d, ожидалось 1", n)
		}
	})
}

// Ревью среза 4, п.3: AcceptRound принял ответ («Всё так»), но markRound не
// смог поправить экран (тот же случай чужого раунда, что и выше, только через
// кнопку, а не текст) - нажатая кнопка снимается, чтобы не остаться рабочей
// после того, как ответ уже учтён.
func TestAllTrueStripsScreenOnMarkFailure(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	ft, tb := newFakeTelegram(t)
	log, _ := screenLog()
	b := screenBot(tb, pool, cases, log)

	cs := startInterview(t, cases, 8802, 1)
	questions := []Question{{Key: "case", Text: "какой заказ?", Suggested: "неважно"}}
	if err := addEvent(ctx, pool, cs.ID, "round_asked",
		map[string]any{"round": 1, "questions": questions}); err != nil {
		t.Fatalf("add round: %v", err)
	}
	// screen_round не совпадает с раундом обращения - markRound откажет, хотя
	// кнопка (round=1 в callback_data) для AcceptRound живая.
	if err := cases.SetScreen(ctx, cs.ID, 100, 0); err != nil {
		t.Fatalf("set screen: %v", err)
	}

	if err := b.onAllTrue(callbackCtx(tb, cs.UserID, 100, "1")); err != nil {
		t.Fatalf("onAllTrue: %v", err)
	}
	if n, err := cases.turnsCount(ctx, pool, cs.ID); err != nil || n != 1 {
		t.Errorf("answer_given после «Всё так»: n=%d err=%v", n, err)
	}
	if len(ft.stripsOf(100)) != 1 {
		t.Errorf("кнопка не снята после неудачной правки экрана: %v", ft.calls)
	}
	if n := len(ft.methodCalls("sendMessage")); n != 1 {
		t.Errorf("ответ не ушёл новым сообщением: сообщений %d", n)
	}
}

// Строка 15 §3b (Р-8): счётчик сбора - такой же живой экран, как раунд или
// саммари, и переживает рестарт точно так же - правкой того же id из БД.
func TestTallyIsScreen(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	const user = int64(8900)
	if _, _, err := cases.StartCase(ctx, User{ID: user, First: "Тест"}, "tg-intake", modeTicket); err != nil {
		t.Fatalf("start case: %v", err)
	}

	ft, tb := newFakeTelegram(t)
	log, _ := screenLog()
	b := screenBot(tb, pool, cases, log)

	if err := b.onItem(textCtx(tb, user, "материал один")); err != nil {
		t.Fatalf("onItem 1: %v", err)
	}
	if n := len(ft.methodCalls("sendMessage")); n != 1 {
		t.Fatalf("счётчик после первого материала: сообщений %d, ожидалось 1", n)
	}
	// Ревью среза 4, п.4: до первого материала экрана не было (screen_msg = 0),
	// и showStep не имеет права снимать кнопки у несуществующего сообщения.
	for _, c := range ft.methodCalls("editMessageReplyMarkup") {
		if fmt.Sprint(c.body["message_id"]) == "0" {
			t.Errorf("stripScreen дёрнул Bot API с message_id 0: %v", c)
		}
	}
	cs, err := cases.Active(ctx, user)
	if err != nil || cs == nil {
		t.Fatalf("active case: %v", err)
	}
	if cs.Screen == 0 {
		t.Fatal("счётчик не стал живым экраном")
	}
	screenID := cs.Screen

	if err := b.onItem(textCtx(tb, user, "материал два")); err != nil {
		t.Fatalf("onItem 2: %v", err)
	}
	if n := len(ft.methodCalls("sendMessage")); n != 1 {
		t.Errorf("второй материал завёл новое сообщение: сообщений %d", n)
	}
	if len(ft.textEditsOf(screenID)) != 1 {
		t.Errorf("счётчик не поправлен на месте: %v", ft.textEditsOf(screenID))
	}

	// «Рестарт»: у нового Bot нет прежней памяти, счётчик живёт только в
	// cases.Screen.
	ft2, tb2 := newFakeTelegram(t)
	log2, _ := screenLog()
	b2 := screenBot(tb2, pool, cases, log2)
	if err := b2.onItem(textCtx(tb2, user, "материал три")); err != nil {
		t.Fatalf("onItem 3: %v", err)
	}
	if n := len(ft2.methodCalls("sendMessage")); n != 0 {
		t.Errorf("третий материал после рестарта завёл новое сообщение: %d", n)
	}
	if len(ft2.textEditsOf(screenID)) != 1 {
		t.Errorf("счётчик после рестарта не поправлен: %v", ft2.textEditsOf(screenID))
	}
	if got := reload(t, cases, cs.ID).Screen; got != screenID {
		t.Errorf("screen_msg сменился: %d, ожидался прежний %d", got, screenID)
	}
}

// Ревью среза 4, п.4: ResetScreen меняет только свои колонки - updated_at
// держит SweepDrafts и RemindDrafts (§3a плана), снятие экрана не должно его
// сдвигать так же, как и SetScreen (TestScreenSurvivesRestart).
func TestResetScreenKeepsUpdatedAt(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	cs, _, err := cases.StartCase(ctx, User{ID: 8950, First: "Тест"}, "tg-intake", modeTicket)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	if err := cases.SetScreen(ctx, cs.ID, 555, 1); err != nil {
		t.Fatalf("set screen: %v", err)
	}
	before := caseUpdatedAt(t, pool, cs.ID)

	if err := cases.ResetScreen(ctx, cs.ID, 555); err != nil {
		t.Fatalf("reset screen: %v", err)
	}
	if after := caseUpdatedAt(t, pool, cs.ID); !after.Equal(before) {
		t.Errorf("updated_at сдвинулся снятием экрана: было %v, стало %v", before, after)
	}
	if fresh := reload(t, cases, cs.ID); fresh.Screen != 0 || fresh.ScreenRound != 0 {
		t.Errorf("экран не снят: screen_msg=%d screen_round=%d", fresh.Screen, fresh.ScreenRound)
	}
}

// Строка 16 §3b: три перехода - новость по тикету дальше не в счёт, здесь
// «Notify keysHome», «Готово» и «Да, сбросить» - снимают живой экран и
// обнуляют screen_msg.
func TestTransitionClosesScreen(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	t.Run("notify keysHome", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 9001, 1)
		if err := cases.SetScreen(ctx, cs.ID, 100, 0); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.Notify(ctx, notifyJob(t, cs.ID, "Что бы вы хотели изменить?", keysHome)); err != nil {
			t.Fatalf("notify: %v", err)
		}
		if len(ft.stripsOf(100)) != 1 {
			t.Errorf("экран не снят при переходе: %v", ft.calls)
		}
		if got := reload(t, cases, cs.ID).Screen; got != 0 {
			t.Errorf("screen_msg после перехода: %d, ожидался 0", got)
		}
	})

	t.Run("готово закрывает сбор", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		const user = int64(9002)
		cs, _, err := cases.StartCase(ctx, User{ID: user, First: "Тест"}, "tg-intake", modeTicket)
		if err != nil {
			t.Fatalf("start case: %v", err)
		}
		if _, err := cases.CollectItem(ctx, tb, cs, &tele.Message{Text: "материал"}); err != nil {
			t.Fatalf("collect item: %v", err)
		}
		if err := cases.SetScreen(ctx, cs.ID, 100, 0); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.onDone(textCtx(tb, user, "Готово")); err != nil {
			t.Fatalf("onDone: %v", err)
		}
		if len(ft.stripsOf(100)) != 1 {
			t.Errorf("счётчик сбора не снят при «Готово»: %v", ft.calls)
		}
		if got := reload(t, cases, cs.ID).Screen; got != 0 {
			t.Errorf("screen_msg после «Готово»: %d, ожидался 0", got)
		}
	})

	t.Run("да сбросить закрывает обращение", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 9003, 1)
		if err := cases.SetScreen(ctx, cs.ID, 100, 1); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.onResetYes(callbackCtx(tb, cs.UserID, 100, cs.ID)); err != nil {
			t.Fatalf("onResetYes: %v", err)
		}
		if len(ft.stripsOf(100)) != 1 {
			t.Errorf("экран не снят при сбросе: %v", ft.calls)
		}
		if got := reload(t, cases, cs.ID).Screen; got != 0 {
			t.Errorf("screen_msg после сброса: %d, ожидался 0", got)
		}
	})
}

// Строки 17-18 §3b (M2, S1): попытка перехода, не изменившая состояние в БД,
// не имеет права снять кнопки живого экрана - ни своя ошибка (нет материала,
// не в сборе, публикация уже идёт), ни гонка с параллельным шагом.
func TestFailedTransitionKeepsScreen(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	t.Run("готово без материала", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		const user = int64(9101)
		cs, _, err := cases.StartCase(ctx, User{ID: user, First: "Тест"}, "tg-intake", modeTicket)
		if err != nil {
			t.Fatalf("start case: %v", err)
		}
		if err := cases.SetScreen(ctx, cs.ID, 100, 0); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.onDone(textCtx(tb, user, "Готово")); err != nil {
			t.Fatalf("onDone: %v", err)
		}
		if len(ft.stripsOf(100)) != 0 {
			t.Errorf("экран снят при отказе «нечего разбирать»: %v", ft.calls)
		}
		if got := reload(t, cases, cs.ID).Screen; got != 100 {
			t.Errorf("screen_msg после отказа: %d, ожидался прежний 100", got)
		}
	})

	t.Run("продолжить в разборе", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 9102, 1)
		if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'summary' WHERE id = $1`, cs.ID); err != nil {
			t.Fatalf("move to summary: %v", err)
		}
		if err := cases.SetScreen(ctx, cs.ID, 100, 0); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.onContinue(callbackCtx(tb, cs.UserID, 200, "tg-intake")); err != nil {
			t.Fatalf("onContinue: %v", err)
		}
		if len(ft.stripsOf(100)) != 0 {
			t.Errorf("экран снят «Продолжить» вне сбора: %v", ft.calls)
		}
		if got := reload(t, cases, cs.ID).Screen; got != 100 {
			t.Errorf("screen_msg: %d, ожидался прежний 100", got)
		}
	})

	t.Run("сброс при публикации", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 9103, 1)
		if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'publishing' WHERE id = $1`, cs.ID); err != nil {
			t.Fatalf("move to publishing: %v", err)
		}
		if err := cases.SetScreen(ctx, cs.ID, 100, 0); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.onResetYes(callbackCtx(tb, cs.UserID, 100, cs.ID)); err != nil {
			t.Fatalf("onResetYes: %v", err)
		}
		if len(ft.stripsOf(100)) != 0 {
			t.Errorf("экран снят при запрете сброса из publishing: %v", ft.calls)
		}
		if got := reload(t, cases, cs.ID).Screen; got != 100 {
			t.Errorf("screen_msg: %d, ожидался прежний 100", got)
		}
	})

	t.Run("экран сменился до closeScreen", func(t *testing.T) {
		_, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 9104, 1)
		if err := cases.SetScreen(ctx, cs.ID, 100, 1); err != nil {
			t.Fatalf("set screen: %v", err)
		}
		stale := reload(t, cases, cs.ID) // помнит screen_msg = 100

		// Конкурентный шаг успел переписать экран, пока решался переход.
		if err := cases.SetScreen(ctx, cs.ID, 200, 2); err != nil {
			t.Fatalf("set screen (race): %v", err)
		}

		b.closeScreen(ctx, stale)

		if got := reload(t, cases, cs.ID).Screen; got != 200 {
			t.Errorf("closeScreen затёр свежий экран: screen_msg=%d, ожидался 200", got)
		}
	})
}

// Строка 19 §3b: новость по тикету и сообщение без кнопок к жизни разговора
// отношения не имеют и не должны трогать живой экран, каким бы он ни был.
func TestNewsKeepsScreen(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	ft, tb := newFakeTelegram(t)
	log, _ := screenLog()
	b := screenBot(tb, pool, cases, log)

	cs := publishCase(t, cases, 9200, 500)
	if err := cases.SetScreen(ctx, cs.ID, 100, 0); err != nil {
		t.Fatalf("set screen: %v", err)
	}

	if err := b.Notify(ctx, notifyJob(t, cs.ID, "Тикет #500: смена статуса", keysTicket)); err != nil {
		t.Fatalf("notify ticket: %v", err)
	}
	if err := b.Notify(ctx, notifyJob(t, cs.ID, "Напоминание без кнопок", "")); err != nil {
		t.Fatalf("notify plain: %v", err)
	}

	touched := append(ft.stripsOf(100), ft.textEditsOf(100)...)
	if len(touched) != 0 {
		t.Errorf("новость или сообщение без кнопок тронули экран: %v", touched)
	}
	if got := reload(t, cases, cs.ID).Screen; got != 100 {
		t.Errorf("screen_msg: %d, ожидался прежний 100", got)
	}
	if n := len(ft.methodCalls("sendMessage")); n != 2 {
		t.Errorf("оба уведомления обязаны дойти новым сообщением: сообщений %d", n)
	}
}

// Строка 20 §3b (S3): кнопка шага отвечает на нажатие всегда, даже когда
// обращения уже нет или его чтение отказало - иначе автор видит вечный
// спиннер.
func TestStepButtonAlwaysAnswers(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	handlers := []struct {
		name    string
		handler func(*Bot, tele.Context) error
	}{
		{"публикую", (*Bot).onPublish},
		{"всё так", (*Bot).onAllTrue},
		{"поправить", (*Bot).onFix},
	}

	t.Run("без активного обращения", func(t *testing.T) {
		for n, h := range handlers {
			t.Run(h.name, func(t *testing.T) {
				ft, tb := newFakeTelegram(t)
				log, _ := screenLog()
				b := screenBot(tb, pool, cases, log)

				user := int64(9300 + n)
				_ = h.handler(b, callbackCtx(tb, user, 1, "1"))
				if len(ft.methodCalls("answerCallbackQuery")) != 1 {
					t.Errorf("%s без обращения не ответил на нажатие: %v", h.name, ft.calls)
				}
			})
		}
	})

	// Отказ Active - не ожидаемый исход, а сбой: хендлер возвращает ошибку и сам
	// не отвечает на callback, чтобы не ответить дважды - второй раз отвечает
	// OnError (bot.go, единственная точка после разбора ошибки хендлера).
	t.Run("active вернул ошибку", func(t *testing.T) {
		for n, h := range handlers {
			t.Run(h.name, func(t *testing.T) {
				badPool := brokenPool(t)
				badCases := newTestCases(t, badPool, t.TempDir())
				ft, tb := newFakeTelegram(t)
				log, _ := screenLog()
				b := screenBot(tb, badPool, badCases, log)

				user := int64(9310 + n)
				err := h.handler(b, callbackCtx(tb, user, 1, "1"))
				if err == nil {
					t.Errorf("%s при отказе Active не вернул ошибку", h.name)
				}
				if len(ft.methodCalls("answerCallbackQuery")) != 0 {
					t.Errorf("%s сам ответил на нажатие при отказе Active: %v - в проде это дубль с OnError", h.name, ft.calls)
				}
			})
		}
	})
}
