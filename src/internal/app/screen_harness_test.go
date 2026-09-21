package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tele "gopkg.in/telebot.v4"
)

// screen_harness_test.go - общая обвязка тестов живого экрана: фейковый
// Telegram по образцу poller_test.go (httptest, без реального Bot API) и Bot,
// собранный напрямую - NewBot тянет старт поллера и регистрацию хендлеров,
// которых тестам не нужно.

// tgCall - один вызов Bot API: метод и раскодированное тело запроса. Значения
// params у telebot всегда JSON-совместимы (map[string]string либо структура с
// json-тегами), поэтому разбор в map[string]any работает для любого метода.
type tgCall struct {
	method string
	body   map[string]any
}

// fakeTelegram пишет каждый вызов Bot API и умеет подменить ответ на него:
// тесты снятия экрана проверяют и порядок вызовов (снятие раньше отправки), и
// поведение при отказе Telegram.
type fakeTelegram struct {
	mu     sync.Mutex
	calls  []tgCall
	nextID int
	// fail проверяется после записи вызова в calls: тест видит вызов, даже
	// если Bot API его отклонил. nil - вызов проходит успешно.
	fail func(tgCall) *tele.Error
}

func newFakeTelegram(t *testing.T) (*fakeTelegram, *tele.Bot) {
	t.Helper()

	ft := &fakeTelegram{nextID: 1000}
	server := httptest.NewServer(http.HandlerFunc(ft.handle))
	t.Cleanup(server.Close)

	tb, err := tele.NewBot(tele.Settings{Token: "test", URL: server.URL, Offline: true})
	if err != nil {
		t.Fatalf("new bot: %v", err)
	}
	return ft, tb
}

func (f *fakeTelegram) handle(w http.ResponseWriter, r *http.Request) {
	method := path.Base(r.URL.Path)
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	call := tgCall{method: method, body: body}
	f.calls = append(f.calls, call)
	var fail *tele.Error
	if f.fail != nil {
		fail = f.fail(call)
	}
	id := f.nextID
	f.nextID++
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if fail != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": false, "error_code": fail.Code, "description": fail.Description,
		})
		return
	}

	if method == "answerCallbackQuery" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		return
	}

	// Правка правит существующее сообщение - id остаётся тем же, а не растёт:
	// без этого повторная правка одного счётчика выглядела бы новым сообщением.
	if raw, ok := body["message_id"]; ok {
		if s, ok := raw.(string); ok {
			if n, err := strconv.Atoi(s); err == nil {
				id = n
			}
		}
	}
	var chatID int64
	if raw, ok := body["chat_id"]; ok {
		chatID, _ = strconv.ParseInt(fmt.Sprint(raw), 10, 64)
	}
	text, _ := body["text"].(string)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{
		"message_id": id,
		"date":       time.Now().Unix(),
		"chat":       map[string]any{"id": chatID, "type": "private"},
		"text":       text,
	}})
}

// methodCalls - вызовы одного метода Bot API, в порядке обращения.
func (f *fakeTelegram) methodCalls(method string) []tgCall {
	var out []tgCall
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

// stripsOf - вызовы editMessageReplyMarkup по конкретному сообщению: это и
// есть снятие кнопок живого экрана, отдельный метод Bot API от правки текста.
func (f *fakeTelegram) stripsOf(msgID int) []tgCall {
	return f.callsOf("editMessageReplyMarkup", msgID)
}

// textEditsOf - вызовы editMessageText по конкретному сообщению.
func (f *fakeTelegram) textEditsOf(msgID int) []tgCall {
	return f.callsOf("editMessageText", msgID)
}

func (f *fakeTelegram) callsOf(method string, msgID int) []tgCall {
	want := strconv.Itoa(msgID)
	var out []tgCall
	for _, c := range f.calls {
		if c.method == method && fmt.Sprint(c.body["message_id"]) == want {
			out = append(out, c)
		}
	}
	return out
}

// indexOf - позиция первого вызова метода в общем порядке, -1 если его не
// было. Нужен там, где важен порядок (снятие раньше отправки).
func (f *fakeTelegram) indexOf(method string) int {
	for n, c := range f.calls {
		if c.method == method {
			return n
		}
	}
	return -1
}

// screenBot собирает Bot напрямую, без NewBot: тестам не нужен ни старт
// поллера, ни регистрация хендлеров, только сам объект с рабочими полями.
// tally и rounds сюда не входят - экран живёт в БД, а не в памяти процесса
// (раздел 1 плана среза), и «новый Bot» в сценариях 3b - это как раз такой
// объект без прежней памяти: свежий screenBot на тех же pool/cases.
func screenBot(tb *tele.Bot, pool *pgxpool.Pool, cases *Cases, log *slog.Logger) *Bot {
	return &Bot{
		bot:       tb,
		pool:      pool,
		cases:     cases,
		log:       log,
		menuAt:    map[int64]time.Time{},
		awaitLink: map[int64]time.Time{},
		waited:    map[int64]string{},
		kills:     map[string]killScreen{},
	}
}

// screenLog - логгер, пишущий в буфер: тесты снятия экрана проверяют факт
// (или отсутствие) записи screen_strip_failed, а не просто отсутствие паники.
func screenLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// callbackCtx - контекст нажатия инлайн-кнопки: сообщение с этим msgID несёт
// кнопку, data - её callback_data (номер раунда, id обращения и т.п.).
func callbackCtx(tb *tele.Bot, userID int64, msgID int, data string) tele.Context {
	return tele.NewContext(tb, tele.Update{
		Callback: &tele.Callback{
			ID:     "cb1",
			Sender: &tele.User{ID: userID},
			Data:   data,
			Message: &tele.Message{
				ID:   msgID,
				Chat: &tele.Chat{ID: userID, Type: tele.ChatPrivate},
			},
		},
	})
}

// textCtx - контекст свободного текстового сообщения автора: ответ в
// интервью, правка саммари.
func textCtx(tb *tele.Bot, userID int64, text string) tele.Context {
	return tele.NewContext(tb, tele.Update{
		Message: &tele.Message{
			ID:     1,
			Chat:   &tele.Chat{ID: userID, Type: tele.ChatPrivate},
			Sender: &tele.User{ID: userID},
			Text:   text,
		},
	})
}

// inlineRows декодирует инлайн-клавиатуру вызова Bot API: telebot кладёт
// reply_markup JSON-строкой внутри тела запроса, а не вложенным объектом.
func inlineRows(t *testing.T, call tgCall) [][]tele.InlineButton {
	t.Helper()

	raw, _ := call.body["reply_markup"].(string)
	if raw == "" {
		return nil
	}
	var markup tele.ReplyMarkup
	if err := json.Unmarshal([]byte(raw), &markup); err != nil {
		t.Fatalf("decode reply_markup: %v", err)
	}
	return markup.InlineKeyboard
}
