package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tele "gopkg.in/telebot.v4"
)

// navigation_test.go - сценарии проверки §3b плана среза «навигация и нижняя
// панель» (docs/plans/plan-navigation-3b.md). Реализации ещё нет: тесты
// пишутся по спеке и сигнатурам раздела 5 плана, поэтому красные до кода - это
// ожидаемо.

// markupRaw декодирует reply_markup в общую карту: тестам навигации нужно
// отличить нижнюю панель (поле "keyboard") от инлайн-экрана ("inline_keyboard"),
// а не только прочитать кнопки.
func markupRaw(t *testing.T, call tgCall) map[string]any {
	t.Helper()

	raw, _ := call.body["reply_markup"].(string)
	if raw == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("decode reply_markup: %v", err)
	}
	return m
}

// replyRows - нижняя панель вызова, аналог inlineRows для "keyboard" вместо
// "inline_keyboard".
func replyRows(t *testing.T, call tgCall) [][]tele.ReplyButton {
	t.Helper()

	raw, _ := call.body["reply_markup"].(string)
	if raw == "" {
		return nil
	}
	var markup tele.ReplyMarkup
	if err := json.Unmarshal([]byte(raw), &markup); err != nil {
		t.Fatalf("decode reply_markup: %v", err)
	}
	return markup.ReplyKeyboard
}

// hasInlineButton - есть ли среди инлайн-кнопок вызова кнопка с таким текстом.
func hasInlineButton(rows [][]tele.InlineButton, text string) bool {
	for _, row := range rows {
		for _, btn := range row {
			if btn.Text == text {
				return true
			}
		}
	}
	return false
}

// assertStale - факт «Stale» из шапки §3b: один answerCallbackQuery с текстом
// «Этот экран устарел», снятие кнопок нажатого сообщения без правки его
// текста, ни новых сообщений, ни работ.
func assertStale(t *testing.T, ft *fakeTelegram, msgID int) {
	t.Helper()

	if got := lastToast(ft); got != "Этот экран устарел" {
		t.Errorf("toast: %q, ожидалось «Этот экран устарел»", got)
	}
	if n := len(ft.methodCalls("answerCallbackQuery")); n != 1 {
		t.Errorf("ответов на нажатие: %d, ожидался 1", n)
	}
	if len(ft.stripsOf(msgID)) != 1 {
		t.Errorf("кнопка %d не снята: %v", msgID, ft.calls)
	}
	if len(ft.textEditsOf(msgID)) != 0 {
		t.Errorf("текст %d тронут: %v", msgID, ft.textEditsOf(msgID))
	}
	if n := len(ft.methodCalls("sendMessage")); n != 0 {
		t.Errorf("новое сообщение при stale: %d", n)
	}
}

// Строка 1 §3b: домашний экран - одно сообщение, на всех трёх входах без
// перехода (правило 4: нижняя панель меняется только сообщением-переходом).
func TestHomeScreenOneMessage(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	addProject(t, pool, "second-proj")

	inputs := []struct {
		name string
		user int64
		call func(*Bot, tele.Context) error
		ctx  func(*tele.Bot, int64) tele.Context
	}{
		{"старт", 20001, (*Bot).onStart, func(tb *tele.Bot, u int64) tele.Context {
			return textCtx(tb, u, "/start")
		}},
		{"меню", 20002, (*Bot).onMenu, func(tb *tele.Bot, u int64) tele.Context {
			return textCtx(tb, u, "Меню")
		}},
		{"свободный текст", 20003, (*Bot).onItem, func(tb *tele.Bot, u int64) tele.Context {
			return textCtx(tb, u, "привет")
		}},
	}

	for _, in := range inputs {
		t.Run(in.name, func(t *testing.T) {
			ft, tb := newFakeTelegram(t)
			log, _ := screenLog()
			b := screenBot(tb, pool, cases, log)

			if err := in.call(b, in.ctx(tb, in.user)); err != nil {
				t.Fatalf("%s: %v", in.name, err)
			}

			sends := ft.methodCalls("sendMessage")
			if len(sends) != 1 {
				t.Fatalf("сообщений: %d, ожидалось 1: %v", len(sends), sends)
			}
			text, _ := sends[0].body["text"].(string)
			if !strings.HasSuffix(text, homeText) {
				t.Errorf("текст: %q, ожидалось окончание %q", text, homeText)
			}
			raw := markupRaw(t, sends[0])
			if _, ok := raw["keyboard"]; ok {
				t.Errorf("домашний экран несёт нижнюю панель: %v", raw)
			}
			// Наличие конкретных кнопок, а не общее число строк: соседние тесты
			// пакета заводят свои проекты, и точный счёт был бы завязан на то,
			// что в базе больше никого нет - ложный сбой без поломки поведения.
			rows := inlineRows(t, sends[0])
			if !hasInlineButton(rows, "Другой second-proj") {
				t.Errorf("нет кнопки добавленного проекта: %v", rows)
			}
			if !hasInlineButton(rows, "Добавить проект") {
				t.Errorf("нет кнопки «Добавить проект»: %v", rows)
			}
		})
	}
}

// Строки 2-4 §3b: переход правит нажатое (где было) и добавляет ровно два
// сообщения - панель, затем домашний экран без панели; «Готово» без
// обращения несёт панель одним сообщением.
func TestTransitionSendsPanel(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	assertPanelThenHome := func(t *testing.T, ft *fakeTelegram) {
		t.Helper()

		sends := ft.methodCalls("sendMessage")
		if len(sends) != 2 {
			t.Fatalf("сообщений: %d, ожидалось 2: %v", len(sends), sends)
		}
		rows := replyRows(t, sends[0])
		if len(rows) != 1 || len(rows[0]) != 2 || rows[0][0].Text != "Меню" || rows[0][1].Text != "Сброс" {
			t.Errorf("панель первым сообщением: %v", sends[0].body)
		}
		if _, ok := markupRaw(t, sends[0])["inline_keyboard"]; ok {
			t.Errorf("панель несёт инлайн-кнопки: %v", sends[0].body)
		}
		text, _ := sends[1].body["text"].(string)
		if text != homeText {
			t.Errorf("второе сообщение: %q, ожидался %q", text, homeText)
		}
		if _, ok := markupRaw(t, sends[1])["keyboard"]; ok {
			t.Errorf("домашний экран несёт нижнюю панель: %v", sends[1].body)
		}
	}

	t.Run("сброс без обращения", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		if err := b.onReset(textCtx(tb, 21001, "Сброс")); err != nil {
			t.Fatalf("onReset: %v", err)
		}
		assertPanelThenHome(t, ft)
	})

	t.Run("сброс при пустом сборе", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		if _, _, err := cases.StartCase(ctx, User{ID: 21002, First: "Тест"}, "tg-intake", modeTicket); err != nil {
			t.Fatalf("start case: %v", err)
		}
		if err := b.onReset(textCtx(tb, 21002, "Сброс")); err != nil {
			t.Fatalf("onReset: %v", err)
		}
		assertPanelThenHome(t, ft)
	})

	t.Run("да сбросить", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		cs, _, err := cases.StartCase(ctx, User{ID: 21003, First: "Тест"}, "tg-intake", modeTicket)
		if err != nil {
			t.Fatalf("start case: %v", err)
		}
		if err := b.onResetYes(callbackCtx(tb, 21003, 50, cs.ID)); err != nil {
			t.Fatalf("onResetYes: %v", err)
		}
		if len(ft.textEditsOf(50)) != 1 {
			t.Fatalf("правка нажатого 50: %v", ft.calls)
		}
		if editIdx, sendIdx := ft.indexOf("editMessageText"), ft.indexOf("sendMessage"); editIdx < 0 || sendIdx < 0 || editIdx > sendIdx {
			t.Fatalf("порядок: правка %d, отправка %d - ожидалась правка раньше", editIdx, sendIdx)
		}
		assertPanelThenHome(t, ft)
	})

	t.Run("закончить разговор", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		if _, _, err := cases.StartCase(ctx, User{ID: 21004, First: "Тест"}, "tg-intake", modeAsk); err != nil {
			t.Fatalf("start case: %v", err)
		}
		if err := b.onEndAsk(callbackCtx(tb, 21004, 60, "")); err != nil {
			t.Fatalf("onEndAsk: %v", err)
		}
		if len(ft.textEditsOf(60)) != 1 {
			t.Fatalf("правка нажатого 60: %v", ft.calls)
		}
		if editIdx, sendIdx := ft.indexOf("editMessageText"), ft.indexOf("sendMessage"); editIdx < 0 || sendIdx < 0 || editIdx > sendIdx {
			t.Fatalf("порядок: правка %d, отправка %d - ожидалась правка раньше", editIdx, sendIdx)
		}
		assertPanelThenHome(t, ft)
	})

	t.Run("готово без обращения", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		if err := b.onDone(textCtx(tb, 21005, "Готово")); err != nil {
			t.Fatalf("onDone: %v", err)
		}
		sends := ft.methodCalls("sendMessage")
		if len(sends) != 1 {
			t.Fatalf("сообщений: %d, ожидалось 1: %v", len(sends), sends)
		}
		rows := replyRows(t, sends[0])
		if len(rows) != 1 || len(rows[0]) != 2 || rows[0][0].Text != "Меню" || rows[0][1].Text != "Сброс" {
			t.Errorf("панель: %v", sends[0].body)
		}
		if _, ok := markupRaw(t, sends[0])["inline_keyboard"]; ok {
			t.Errorf("«Готово» без обращения несёт инлайн-кнопки: %v", sends[0].body)
		}
	})
}

// Строки 5-8 §3b (Р-8): исход отмены тикета правит карточку по cases.screen_msg,
// а не по памяти процесса - переживает рестарт и повторную доставку; отказ
// правки уходит запасным сообщением по правилу 2.
func TestKillOutcomeOnCard(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	tickets := newTestTickets(t, cases, "http://unused.invalid")
	project := testProject(t, pool)

	t.Run("отмена и повтор исхода", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log, tickets)

		cs := publishCase(t, cases, 22001, 7)

		if err := b.onKill(callbackCtx(tb, cs.UserID, 300, cardData(project.Slug, 7))); err != nil {
			t.Fatalf("onKill: %v", err)
		}
		if got := lastToast(ft); got != "Отменяю тикет" {
			t.Errorf("toast нажатия: %q, ожидалось «Отменяю тикет»", got)
		}
		if len(ft.textEditsOf(300)) != 1 {
			t.Fatalf("правка карточки нажатием: %v", ft.calls)
		}
		if got := reload(t, cases, cs.ID).Screen; got != 300 {
			t.Errorf("screen_msg после «Отменить тикет»: %d, ожидался 300", got)
		}
		if n := countJobs(t, pool, JobCancelIssue, cs.ID); n != 1 {
			t.Errorf("работ cancel_issue: %d, ожидалась 1", n)
		}

		// Исход приходит очередью с нового Bot: экран ищется в cases.screen_msg,
		// а не в памяти процесса, поставившего работу.
		ft2, tb2 := newFakeTelegram(t)
		log2, _ := screenLog()
		b2 := screenBot(tb2, pool, cases, log2, tickets)

		if err := b2.Notify(ctx, notifyJob(t, cs.ID, "Тикет #7 отменён и закрыт.", keysCancel)); err != nil {
			t.Fatalf("notify 1: %v", err)
		}
		if len(ft2.textEditsOf(300)) != 1 {
			t.Fatalf("правка карточки 300: %v", ft2.calls)
		}
		edit := ft2.textEditsOf(300)[0]
		if text, _ := edit.body["text"].(string); text != "Тикет #7 отменён и закрыт." {
			t.Errorf("текст исхода: %q", text)
		}
		rows := inlineRows(t, edit)
		if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0].Text != "К списку" {
			t.Fatalf("кнопка исхода: %v", rows)
		}
		if len(ft2.methodCalls("sendMessage")) != 0 {
			t.Errorf("исход ушёл новым сообщением: %v", ft2.calls)
		}

		// Повтор notify (доставка работы дважды) правит ту же карточку ещё раз.
		if err := b2.Notify(ctx, notifyJob(t, cs.ID, "Тикет #7 отменён и закрыт.", keysCancel)); err != nil {
			t.Fatalf("notify 2: %v", err)
		}
		if len(ft2.textEditsOf(300)) != 2 {
			t.Errorf("повтор не поправил карточку: %v", ft2.calls)
		}
		if len(ft2.methodCalls("sendMessage")) != 0 {
			t.Errorf("повтор ушёл новым сообщением: %v", ft2.calls)
		}
	})

	t.Run("экран не записан", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log, tickets)

		// Отмена состоялась до этого среза: screen_msg остался 0.
		cs := publishCase(t, cases, 22002, 8)

		if err := b.Notify(ctx, notifyJob(t, cs.ID, "Тикет #8 отменён и закрыт.", keysCancel)); err != nil {
			t.Fatalf("notify: %v", err)
		}
		sends := ft.methodCalls("sendMessage")
		if len(sends) != 1 {
			t.Fatalf("сообщений: %d, ожидалось 1: %v", len(sends), sends)
		}
		if text, _ := sends[0].body["text"].(string); text != "Тикет #8 отменён и закрыт." {
			t.Errorf("текст: %q", text)
		}
		rows := inlineRows(t, sends[0])
		if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0].Text != "К списку" {
			t.Fatalf("кнопка: %v", rows)
		}
		if len(ft.methodCalls("editMessageText")) != 0 {
			t.Errorf("правка без экрана: %v", ft.calls)
		}
	})

	t.Run("правка карточки не удалась", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		ft.fail = func(c tgCall) *tele.Error {
			if c.method == "editMessageText" && fmt.Sprint(c.body["message_id"]) == "300" {
				return tele.NewError(400, "Bad Request: message to edit not found")
			}
			return nil
		}
		b := screenBot(tb, pool, cases, log, tickets)

		cs := publishCase(t, cases, 22003, 9)
		if err := cases.SetScreen(ctx, cs.ID, 300, 0); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.Notify(ctx, notifyJob(t, cs.ID, "Тикет #9 отменён и закрыт.", keysCancel)); err != nil {
			t.Fatalf("notify: %v", err)
		}
		sends := ft.methodCalls("sendMessage")
		if len(sends) != 1 {
			t.Fatalf("сообщений: %d, ожидалось 1: %v", len(sends), sends)
		}
		rows := inlineRows(t, sends[0])
		if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0].Text != "К списку" {
			t.Fatalf("кнопка запасного сообщения: %v", rows)
		}
		if len(ft.stripsOf(300)) != 1 {
			t.Errorf("кнопки старой карточки не сняты: %v", ft.calls)
		}
		// M2, как у onFix: screen_msg переезжает на запасное сообщение, иначе
		// повтор notify правил бы карточку, которую автор больше не видит.
		if fresh := reload(t, cases, cs.ID).Screen; fresh == 0 || fresh == 300 {
			t.Errorf("screen_msg после запасного пути: %d, ожидался новый id", fresh)
		}
	})
}

// Строка 9 §3b: исход отмены A трогает только карточку A - разговор B со
// своим живым экраном и панелью не задет.
func TestKillOutcomeKeepsActiveCase(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	ft, tb := newFakeTelegram(t)
	log, _ := screenLog()
	b := screenBot(tb, pool, cases, log)

	a := publishCase(t, cases, 23001, 10)
	if err := cases.SetScreen(ctx, a.ID, 300, 0); err != nil {
		t.Fatalf("set screen a: %v", err)
	}
	bCase := startInterview(t, cases, 23002, 1)
	if err := cases.SetScreen(ctx, bCase.ID, 100, 1); err != nil {
		t.Fatalf("set screen b: %v", err)
	}

	if err := b.Notify(ctx, notifyJob(t, a.ID, "Тикет #10 отменён и закрыт.", keysCancel)); err != nil {
		t.Fatalf("notify: %v", err)
	}

	if len(ft.calls) != 1 {
		t.Fatalf("вызовов Bot API: %d, ожидался 1: %v", len(ft.calls), ft.calls)
	}
	call := ft.calls[0]
	if call.method != "editMessageText" || fmt.Sprint(call.body["message_id"]) != "300" {
		t.Errorf("вызов: %v, ожидалась правка сообщения 300", call)
	}
	if _, ok := markupRaw(t, call)["keyboard"]; ok {
		t.Errorf("исход нёс нижнюю панель: %v", call.body)
	}
	if got := reload(t, cases, bCase.ID).Screen; got != 100 {
		t.Errorf("screen_msg разговора B: %d, ожидался прежний 100", got)
	}
}

// Строка 10 §3b: чужое нажатие «Отменить тикет» не пишет screen_msg и не
// ставит работу отмены - право проверяется раньше записи.
func TestKillScreenOnlyOnCancel(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	tickets := newTestTickets(t, cases, "http://unused.invalid")
	project := testProject(t, pool)
	ft, tb := newFakeTelegram(t)
	log, _ := screenLog()
	b := screenBot(tb, pool, cases, log, tickets)

	cs := publishCase(t, cases, 24001, 11)

	if err := b.onKill(callbackCtx(tb, 24002, 300, cardData(project.Slug, 11))); err != nil {
		t.Fatalf("onKill: %v", err)
	}

	if got := reload(t, cases, cs.ID).Screen; got != 0 {
		t.Errorf("screen_msg после чужого нажатия: %d, ожидался 0", got)
	}
	if n := countJobs(t, pool, JobCancelIssue, cs.ID); n != 0 {
		t.Errorf("работа cancel_issue поставлена по чужому нажатию: %d", n)
	}
	if len(ft.textEditsOf(300)) != 1 {
		t.Fatalf("правка отказа 300: %v", ft.calls)
	}
	if text, _ := ft.textEditsOf(300)[0].body["text"].(string); text != "Отменить тикет может только его автор." {
		t.Errorf("текст отказа: %q", text)
	}
}

// Строки 11-13 §3b (правило 2): правка навигации, которая не удалась, уходит
// запасным сообщением со снятием кнопок старого экрана; «уже не изменено» (оба
// варианта текста Bot API) - успех без повторной отправки; текст длиннее
// предела правкой не помещается вовсе.
func TestNavigationEditFallback(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	t.Run("нельзя редактировать", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, buf := screenLog()
		ft.fail = func(c tgCall) *tele.Error {
			if c.method == "editMessageText" && fmt.Sprint(c.body["message_id"]) == "50" {
				return tele.ErrCantEditMessage
			}
			return nil
		}
		b := screenBot(tb, pool, cases, log)

		if err := b.onHome(callbackCtx(tb, 25001, 50, "")); err != nil {
			t.Fatalf("onHome: %v", err)
		}
		sends := ft.methodCalls("sendMessage")
		if len(sends) != 1 {
			t.Fatalf("сообщений: %d, ожидалось 1: %v", len(sends), sends)
		}
		if text, _ := sends[0].body["text"].(string); text != homeText {
			t.Errorf("текст запасного сообщения: %q, ожидался %q", text, homeText)
		}
		if len(ft.stripsOf(50)) != 1 {
			t.Errorf("кнопки старого экрана не сняты: %v", ft.calls)
		}
		if !strings.Contains(buf.String(), "screen_edit_failed") {
			t.Error("отказ правки не залогирован")
		}
	})

	for _, tc := range []struct {
		name string
		err  *tele.Error
	}{
		{"уже не изменено", tele.ErrMessageNotModified},
		{"тот же текст и разметка", tele.ErrSameMessageContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ft, tb := newFakeTelegram(t)
			log, _ := screenLog()
			ft.fail = func(c tgCall) *tele.Error {
				if c.method == "editMessageText" && fmt.Sprint(c.body["message_id"]) == "50" {
					return tc.err
				}
				return nil
			}
			b := screenBot(tb, pool, cases, log)

			if err := b.onHome(callbackCtx(tb, 25002, 50, "")); err != nil {
				t.Fatalf("onHome: %v", err)
			}
			if n := len(ft.methodCalls("sendMessage")); n != 0 {
				t.Errorf("двойное нажатие завело новое сообщение: %d", n)
			}
			if n := len(ft.stripsOf(50)); n != 0 {
				t.Errorf("двойное нажатие сняло кнопки: %d", n)
			}
		})
	}

	t.Run("длинный экран", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		markup := &tele.ReplyMarkup{}
		markup.Inline(markup.Row(markup.Data("К списку", ticketsBtn.Unique, listData("tg-intake", 0))))
		text := strings.Repeat("а", 4500)

		sent, err := b.editScreen(&tele.User{ID: 25003},
			tele.StoredMessage{MessageID: "50", ChatID: 25003}, text, markup)
		if err != nil {
			t.Fatalf("editScreen: %v", err)
		}
		if sent == nil {
			t.Fatal("editScreen не вернул отправленное сообщение")
		}
		if len(ft.textEditsOf(50)) != 0 {
			t.Errorf("длинный текст ушёл правкой: %v", ft.calls)
		}
		sends := ft.methodCalls("sendMessage")
		if len(sends) != 2 {
			t.Fatalf("сообщений: %d, ожидалось 2 (текст режется на два)", len(sends))
		}
		if _, ok := sends[0].body["reply_markup"]; ok {
			t.Errorf("кнопки на первом куске: %v", sends[0].body)
		}
		if _, ok := sends[1].body["reply_markup"]; !ok {
			t.Errorf("кнопок нет на последнем куске: %v", sends[1].body)
		}
		if len(ft.stripsOf(50)) != 1 {
			t.Errorf("старый экран не снят: %v", ft.calls)
		}
	})
}

// Строка 14 §3b (M2): отказ правки экрана саммари уходит новым сообщением, и
// screen_msg переезжает на него - иначе «Публикую» на старом, но всё ещё
// видимом сообщении читалось бы как живая кнопка.
func TestFixFallbackSetsScreen(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	ft, tb := newFakeTelegram(t)
	log, _ := screenLog()
	ft.fail = func(c tgCall) *tele.Error {
		if c.method == "editMessageText" && fmt.Sprint(c.body["message_id"]) == "100" {
			return tele.ErrCantEditMessage
		}
		return nil
	}
	b := screenBot(tb, pool, cases, log)

	cs := startInterview(t, cases, 26001, 2)
	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'summary' WHERE id = $1`, cs.ID); err != nil {
		t.Fatalf("move to summary: %v", err)
	}
	if err := cases.SetScreen(ctx, cs.ID, 100, 0); err != nil {
		t.Fatalf("set screen: %v", err)
	}

	if err := b.onFix(callbackCtx(tb, cs.UserID, 100, "")); err != nil {
		t.Fatalf("onFix: %v", err)
	}

	sends := ft.methodCalls("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("сообщений: %d, ожидалось 1: %v", len(sends), sends)
	}
	rows := inlineRows(t, sends[0])
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0].Text != "Публикую" {
		t.Fatalf("кнопка на новом экране: %v", rows)
	}
	if len(ft.stripsOf(100)) != 1 {
		t.Errorf("старый экран не снят: %v", ft.calls)
	}

	fresh := reload(t, cases, cs.ID)
	if fresh.Screen == 0 || fresh.Screen == 100 {
		t.Errorf("screen_msg после запасного пути: %d, ожидался новый id", fresh.Screen)
	}
}

// Строка 15 §3b (правило 3): навигация («К проектам», «Назад») не сверяется с
// живым экраном обращения и не имеет права его снять - это привилегия шага.
func TestNavigationKeepsLiveScreen(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	ft, tb := newFakeTelegram(t)
	log, _ := screenLog()
	b := screenBot(tb, pool, cases, log)

	cs := startInterview(t, cases, 27001, 1)
	if err := cases.SetScreen(ctx, cs.ID, 100, 1); err != nil {
		t.Fatalf("set screen: %v", err)
	}

	if err := b.onHome(callbackCtx(tb, cs.UserID, 50, "")); err != nil {
		t.Fatalf("onHome: %v", err)
	}
	if err := b.onProjectMenu(callbackCtx(tb, cs.UserID, 50, "tg-intake")); err != nil {
		t.Fatalf("onProjectMenu: %v", err)
	}

	if len(ft.stripsOf(100)) != 0 {
		t.Errorf("живой экран разговора снят навигацией: %v", ft.calls)
	}
	if got := reload(t, cases, cs.ID).Screen; got != 100 {
		t.Errorf("screen_msg: %d, ожидался прежний 100", got)
	}
}

// Строки 16-18 §3b (правило 3, M1): устаревшая кнопка шага, битые данные
// карточки и подтверждение чужого обращения дают один и тот же факт - toast
// «Этот экран устарел» и снятие нажатой кнопки, без второго ответа на
// callback и без работы.
func TestStaleButtonRefused(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	t.Run("данные не число", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 28001, 1)
		if err := cases.SetScreen(ctx, cs.ID, 100, 1); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.onAllTrue(callbackCtx(tb, cs.UserID, 100, "x")); err != nil {
			t.Fatalf("onAllTrue: %v", err)
		}
		assertStale(t, ft, 100)
		if n, err := cases.turnsCount(ctx, pool, cs.ID); err != nil || n != 0 {
			t.Errorf("answer_given по битым данным: n=%d err=%v", n, err)
		}
	})

	t.Run("раунд устарел", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		// Обращение уже на раунде 2, кнопка раунда 1 осталась в чате на живом
		// экране (screen_msg правится следующим шагом, а не этой кнопкой).
		cs := startInterview(t, cases, 28002, 2)
		if err := cases.SetScreen(ctx, cs.ID, 100, 2); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.onAllTrue(callbackCtx(tb, cs.UserID, 100, "1")); err != nil {
			t.Fatalf("onAllTrue: %v", err)
		}
		assertStale(t, ft, 100)
		if n, err := cases.turnsCount(ctx, pool, cs.ID); err != nil || n != 0 {
			t.Errorf("answer_given по устаревшему раунду: n=%d err=%v", n, err)
		}
	})

	t.Run("обращение не в интервью", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 28003, 1)
		if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'summary' WHERE id = $1`, cs.ID); err != nil {
			t.Fatalf("move to summary: %v", err)
		}
		if err := cases.SetScreen(ctx, cs.ID, 100, 0); err != nil {
			t.Fatalf("set screen: %v", err)
		}

		if err := b.onAllTrue(callbackCtx(tb, cs.UserID, 100, "0")); err != nil {
			t.Fatalf("onAllTrue: %v", err)
		}
		assertStale(t, ft, 100)
	})

	t.Run("битые данные карточки", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		if err := b.onCard(callbackCtx(tb, 28004, 40, "p")); err != nil {
			t.Fatalf("onCard: %v", err)
		}
		assertStale(t, ft, 40)
	})

	t.Run("битые данные отмены", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		if err := b.onKill(callbackCtx(tb, 28005, 40, "p:x")); err != nil {
			t.Fatalf("onKill: %v", err)
		}
		assertStale(t, ft, 40)

		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE kind = $1`, JobCancelIssue).Scan(&n); err != nil {
			t.Fatalf("count jobs: %v", err)
		}
		if n != 0 {
			t.Errorf("работа отмены поставлена по битым данным: %d", n)
		}
	})

	t.Run("подтверждение чужого обращения", func(t *testing.T) {
		ft, tb := newFakeTelegram(t)
		log, _ := screenLog()
		b := screenBot(tb, pool, cases, log)

		cs := startInterview(t, cases, 28006, 1)

		if err := b.onResetYes(callbackCtx(tb, cs.UserID, 50, "другое-обращение")); err != nil {
			t.Fatalf("onResetYes: %v", err)
		}
		assertStale(t, ft, 50)
		if got := reload(t, cases, cs.ID).Status; got != statusInterview {
			t.Errorf("статус после устаревшего подтверждения: %s, ожидался %s", got, statusInterview)
		}
	})
}

// Строка 19 §3b: кнопка прежней версии (незнакомый callback) отвечает тем же
// stale, а не вечным спиннером - маршрут tele.OnCallback в NewBot тестом не
// покрыт (он поднимает поллер), поэтому staleButton зовётся напрямую.
// TestUnknownButtonStale проходит настоящей маршрутизацией telebot
// (b.routes + tb.ProcessUpdate), а не прямым вызовом staleButton: только так
// тест ловит и потерю привязки tele.OnCallback к staleButton в routes, а не
// только поведение самой функции. База не нужна - staleButton её не читает.
func TestUnknownButtonStale(t *testing.T) {
	ft, tb := newFakeTelegram(t)
	log, _ := screenLog()
	b := screenBot(tb, nil, nil, log)
	b.routes(tb)

	tb.ProcessUpdate(tele.Update{Callback: &tele.Callback{
		ID:      "1",
		Sender:  &tele.User{ID: 29001},
		Data:    "\frestart|",
		Message: &tele.Message{ID: 70, Chat: &tele.Chat{ID: 29001, Type: tele.ChatPrivate}},
	}})

	assertStale(t, ft, 70)
}
