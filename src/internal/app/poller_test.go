package app

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tele "gopkg.in/telebot.v4"
)

// TestPollFailureVisible: отказ опроса обязан быть виден. Своего поллера здесь
// не было бы, если бы telebot отдавал ошибку getUpdates хоть куда-то: он кладёт
// её в debug, тот молчит без Verbose, и 409 от второго поллера с тем же токеном
// не оставлял следа ни в логе, ни снаружи.
func TestPollFailureVisible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ровно то, чем Telegram отвечает второму поллеру. Ответ идёт с кодом
		// 200: отказ виден только по полю ok, и разбирать его обязан клиент.
		_, _ = io.WriteString(w, `{"ok":false,"error_code":409,`+
			`"description":"Conflict: terminated by other getUpdates request"}`)
	}))
	defer server.Close()

	bot, err := tele.NewBot(tele.Settings{Token: "test", URL: server.URL, Offline: true})
	if err != nil {
		t.Fatalf("new bot: %v", err)
	}

	p := &poller{log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		alivePath: filepath.Join(t.TempDir(), "poller.alive")}
	if _, err = p.fetch(bot); err == nil {
		t.Fatal("отказ getUpdates принят за пустую пачку апдейтов")
	}
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("причина отказа потеряна: %v", err)
	}
	if _, err := os.Stat(p.alivePath); err == nil {
		t.Error("отметка живости обновлена при отказе опроса")
	}
}

// TestPollerAlive: отметку живости ставит только успешный опрос, и она
// протухает. Healthcheck запускается отдельным процессом и памяти сервиса не
// видит - файл единственное, по чему он отличает работающий поллер от
// молчащего.
func TestPollerAlive(t *testing.T) {
	dir := t.TempDir()
	if PollerAlive(dir) {
		t.Error("поллер признан живым без единого успешного опроса")
	}

	p := &poller{log: slog.New(slog.NewTextHandler(io.Discard, nil)), alivePath: alivePath(dir)}
	p.mark()
	if !PollerAlive(dir) {
		t.Fatal("свежая отметка не считается живой")
	}

	stale := time.Now().Add(-2 * aliveTTL)
	if err := os.Chtimes(alivePath(dir), stale, stale); err != nil {
		t.Fatalf("состарить отметку: %v", err)
	}
	if PollerAlive(dir) {
		t.Error("протухшая отметка считается живой: молчащий поллер остался бы healthy")
	}
}
