package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

var testStatuses = Statuses{
	{Label: "status:cancelled", Title: "Отменён", Final: true},
	{Label: "status:prod", Title: "В проде", Final: true},
	{Label: "status:in-progress", Title: "В работе"},
	{Label: "status:new", Title: "Заведён"},
}

// TestListSkipsPullRequests: REST GitHub считает issue каждый pull request, и
// без отсева метки чужого PR оказались бы статусом нашего тикета.
func TestListSkipsPullRequests(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	publishCase(t, cases, 7001, 12)

	server := githubStub(t, map[string]string{
		"GET /repos/daniil4545/tg-intake/issues": `[
			{"number": 13, "pull_request": {"url": "x"}, "labels": [{"name": "status:prod"}]},
			{"number": 12, "labels": [{"name": "type:bug"}, {"name": "status:in-progress"}]}]`,
	})
	tickets := newTestTickets(t, cases, server.URL)

	list, err := tickets.List(context.Background(), testProject(t, pool))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("тикетов: %d, ожидался 1", len(list))
	}
	if list[0].Status.Label != "status:in-progress" {
		t.Errorf("статус: %q", list[0].Status.Label)
	}
}

// TestListSurvivesGitHubError: просмотр не должен умирать вместе с чужим
// сервисом - номера и заголовки уже прочитаны из своей базы.
func TestListSurvivesGitHubError(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	publishCase(t, cases, 7002, 21)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	tickets := newTestTickets(t, cases, server.URL)

	list, err := tickets.List(context.Background(), testProject(t, pool))
	if err != nil {
		t.Fatalf("отказ GitHub уронил список: %v", err)
	}
	if len(list) != 1 || list[0].Number != 21 {
		t.Fatalf("список: %+v", list)
	}
	if list[0].Status.Label != "" {
		t.Errorf("статус не мог прочитаться, а он есть: %q", list[0].Status.Label)
	}
}

// TestCancelRemovesAllStatusLabels: отмена ставит свою метку и снимает все
// прочие метки статуса точечно. PUT затёр бы и type, и author, проставленные
// не нами.
func TestCancelRemovesAllStatusLabels(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := publishCase(t, cases, 7003, 30)

	seen := &requestLog{}
	server := recordingStub(t, seen, map[string]string{
		"GET /repos/daniil4545/tg-intake/issues/30": `{"number": 30, "labels": [
			{"name": "type:bug"}, {"name": "status:in-progress"}, {"name": "status:new"}]}`,
	})
	tickets := newTestTickets(t, cases, server.URL)

	job := Job{ID: 1, Kind: JobCancelIssue, Payload: cancelJSON(cs.ID, 7003)}
	if err := tickets.RunCancel(context.Background(), job); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	want := []string{
		"POST /repos/daniil4545/tg-intake/issues/30/labels",
		"DELETE /repos/daniil4545/tg-intake/issues/30/labels/status:in-progress",
		"DELETE /repos/daniil4545/tg-intake/issues/30/labels/status:new",
		"PATCH /repos/daniil4545/tg-intake/issues/30",
	}
	for _, w := range want {
		if !seen.has(w) {
			t.Errorf("не было запроса %s; было: %v", w, seen.list())
		}
	}
	for _, got := range seen.list() {
		if strings.HasPrefix(got, "PUT ") {
			t.Errorf("замена набора меток вместо точечной правки: %s", got)
		}
	}
	if n := countJobs(t, pool, JobNotify, cs.ID); n != 1 {
		t.Errorf("уведомлений автору: %d, ожидалось 1", n)
	}
}

// TestCancelDeniedForStranger: просмотр общий, отмена авторская. Кнопка живёт в
// чужом сообщении дольше, чем экран, поэтому право проверяется и в работе.
func TestCancelDeniedForStranger(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := publishCase(t, cases, 7004, 40)

	seen := &requestLog{}
	server := recordingStub(t, seen, nil)
	tickets := newTestTickets(t, cases, server.URL)

	job := Job{ID: 1, Kind: JobCancelIssue, Payload: cancelJSON(cs.ID, 9999)}
	if err := tickets.RunCancel(context.Background(), job); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := seen.list(); len(got) != 0 {
		t.Errorf("чужая отмена дошла до GitHub: %v", got)
	}

	if err := tickets.Cancel(context.Background(), testProject(t, pool), 40, 9999); err != ErrNotAuthor {
		t.Errorf("постановка чужой отмены: %v", err)
	}
}

// TestCancelTwiceOneEvent: повтор работы после падения между закрытием issue и
// гашением работы не должен писать второе событие в журнал.
func TestCancelTwiceOneEvent(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cs := publishCase(t, cases, 7005, 50)

	server := githubStub(t, map[string]string{
		"GET /repos/daniil4545/tg-intake/issues/50": `{"number": 50, "labels": [{"name": "status:new"}]}`,
	})
	tickets := newTestTickets(t, cases, server.URL)

	job := Job{ID: 1, Kind: JobCancelIssue, Payload: cancelJSON(cs.ID, 7005)}
	for range 2 {
		if err := tickets.RunCancel(context.Background(), job); err != nil {
			t.Fatalf("cancel: %v", err)
		}
	}

	var events int
	err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM case_events WHERE case_id = $1 AND kind = 'cancelled_by_author'`,
		cs.ID).Scan(&events)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Errorf("событий отмены: %d, ожидалось 1", events)
	}
}

func newTestTickets(t *testing.T, cases *Cases, api string) *Tickets {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewTickets(cases, NewGitHub("token", api, testStatuses, log), testStatuses, log)
}

// publishCase доводит обращение до опубликованного тикета: просмотр работает
// только с ними.
func publishCase(t *testing.T, cases *Cases, userID int64, issue int) *Case {
	t.Helper()
	ctx := context.Background()

	cs, _, err := cases.StartCase(ctx, User{ID: userID, First: "Тест"}, "tg-intake")
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	_, err = cases.pool.Exec(ctx, `
		UPDATE cases SET status = 'published', kind = 'bug', title = 'Форма не сохраняется',
		                 summary = '## Случай' || chr(10) || 'заказ 4821', issue_number = $2::int,
		                 issue_url = 'https://example.invalid/issues/' || $2::int
		WHERE id = $1`, cs.ID, issue)
	if err != nil {
		t.Fatalf("publish case: %v", err)
	}
	return reload(t, cases, cs.ID)
}

func testProject(t *testing.T, pool *pgxpool.Pool) Project {
	t.Helper()
	projects, err := ListProjects(context.Background(), pool)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) == 0 {
		t.Fatal("в базе нет проектов: миграция seed не применена")
	}
	return projects[0]
}

func cancelJSON(caseID string, userID int64) []byte {
	raw, _ := json.Marshal(cancelPayload{CaseID: caseID, UserID: userID})
	return raw
}

// requestLog копит «метод путь» запросов: тесты отмены проверяют не ответ
// GitHub, а то, какие запросы ушли и в каком виде.
type requestLog struct{ seen []string }

func (r *requestLog) add(method, path string) { r.seen = append(r.seen, method+" "+path) }
func (r *requestLog) list() []string          { return r.seen }
func (r *requestLog) has(want string) bool {
	for _, got := range r.seen {
		if got == want {
			return true
		}
	}
	return false
}

func githubStub(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	return recordingStub(t, &requestLog{}, routes)
}

// recordingStub отвечает по карте «метод путь» и записывает всё, что пришло.
// Неизвестный путь - пустой JSON-массив: отмене хватает, а список без своих
// номеров просто останется без статусов.
func recordingStub(t *testing.T, seen *requestLog, routes map[string]string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, _, _ := strings.Cut(r.URL.RequestURI(), "?")
		seen.add(r.Method, path)

		w.Header().Set("Content-Type", "application/json")
		if body, ok := routes[fmt.Sprintf("%s %s", r.Method, path)]; ok {
			fmt.Fprint(w, body)
			return
		}
		fmt.Fprint(w, "[]")
	}))
	t.Cleanup(server.Close)
	return server
}
