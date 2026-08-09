package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestParseProjectRef: ссылку копируют со страницы репозитория, поэтому она
// приходит и с хвостом, и с .git, и без схемы вовсе.
func TestParseProjectRef(t *testing.T) {
	cases := []struct {
		name        string
		args        string
		owner, repo string
		title, ctx  string
		bad         bool
	}{
		{name: "полная ссылка", args: "https://github.com/daniil4545/planerka", owner: "daniil4545", repo: "planerka"},
		{name: "ссылка с .git", args: "https://github.com/daniil4545/planerka.git", owner: "daniil4545", repo: "planerka"},
		{name: "ссылка с хвостом", args: "https://github.com/daniil4545/planerka/issues/12", owner: "daniil4545", repo: "planerka"},
		{name: "короткая форма", args: "daniil4545/planerka", owner: "daniil4545", repo: "planerka"},
		{name: "явные поля", args: "daniil4545/planerka Планёрка | Транскрипт встреч",
			owner: "daniil4545", repo: "planerka", title: "Планёрка", ctx: "Транскрипт встреч"},
		{name: "только название", args: "daniil4545/planerka Планёрка",
			owner: "daniil4545", repo: "planerka", title: "Планёрка"},
		{name: "пусто", args: "   ", bad: true},
		{name: "без репозитория", args: "https://github.com/daniil4545", bad: true},
		{name: "мусор", args: "заведи проект", bad: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, err := parseProjectRef(c.args)
			if c.bad {
				if err == nil {
					t.Fatalf("want error, got %+v", ref)
				}
				return
			}
			if err != nil {
				t.Fatalf("want no error, got: %v", err)
			}
			if ref.Owner != c.owner || ref.Repo != c.repo {
				t.Errorf("адрес: %s/%s", ref.Owner, ref.Repo)
			}
			if ref.Title != c.title || ref.Context != c.ctx {
				t.Errorf("поля: %q | %q", ref.Title, ref.Context)
			}
		})
	}
}

// TestAddProjectUsesExplicitFields: заданное автором берётся как есть, модель и
// README не трогаются вовсе.
func TestAddProjectUsesExplicitFields(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cleanupProject(t, pool, "planerka")

	seen := &requestLog{}
	server := recordingStub(t, seen, map[string]string{
		"GET /repos/daniil4545/planerka": `{"name": "planerka", "full_name": "daniil4545/planerka",
			"description": "из репозитория", "private": true}`,
	})
	projects := newTestProjects(t, cases, server.URL, "")

	got, source, err := projects.Add(context.Background(), 501,
		"https://github.com/daniil4545/planerka Планёрка | Транскрипт и конспект встреч")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if got.Title != "Планёрка" || got.Context != "Транскрипт и конспект встреч" {
		t.Errorf("поля перезаписаны: %+v", got)
	}
	if source != "автор" {
		t.Errorf("источник описания: %q", source)
	}
	for _, path := range seen.list() {
		if strings.Contains(path, "/readme") {
			t.Error("README читался, хотя описание задано автором")
		}
	}
	if !seen.has("POST /repos/daniil4545/planerka/labels") {
		t.Errorf("право на запись не проверялось: %v", seen.list())
	}
	// Полный bootstrap здесь запрещён: десять меток рабочим клиентом держали бы
	// очередь апдейтов всех авторов.
	labelCalls := 0
	for _, got := range seen.list() {
		if strings.HasSuffix(got, "/labels") {
			labelCalls++
		}
	}
	if labelCalls != 1 {
		t.Errorf("запросов на метки: %d, ожидался один проверочный", labelCalls)
	}
	assertProjectSaved(t, pool, "planerka", "Планёрка")
}

// TestAddProjectDeniedRepo: репозиторий вне доступа токена не должен появиться
// в меню - иначе автор потратит на интервью пять минут и упрётся в отказ.
func TestAddProjectDeniedRepo(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cleanupProject(t, pool, "secret")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message": "Not Found"}`)
	}))
	t.Cleanup(server.Close)
	projects := newTestProjects(t, cases, server.URL, "")

	if _, _, err := projects.Add(context.Background(), 502, "daniil4545/secret"); err == nil {
		t.Fatal("want error, got nil")
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM projects WHERE slug = 'secret'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Error("недоступный репозиторий сохранён проектом")
	}
}

// TestAddProjectFallsBack: отказ модели не роняет команду. Название берётся из
// имени репозитория, контекст - из его описания.
func TestAddProjectFallsBack(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cleanupProject(t, pool, "qualifier")

	github := githubStub(t, map[string]string{
		"GET /repos/daniil4545/qualifier": `{"name": "qualifier", "full_name": "daniil4545/qualifier",
			"description": "Квалификация лидов в Telegram", "private": true}`,
		"GET /repos/daniil4545/qualifier/readme": `{"content": "IyBxdWFsaWZpZXIK", "encoding": "base64"}`,
	})
	// Отказ модели: клиент OpenRouter ходит через прокси, и тестовый прокси
	// отказывает на любой запрос. Другой точки внедрения у клиента нет.
	//
	// Отказ на CONNECT клиент видит как сетевую ошибку и тратит на неё повторы -
	// отсюда несколько секунд на этом тесте.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(broken.Close)

	projects := newTestProjects(t, cases, github.URL, broken.URL)
	got, source, err := projects.Add(context.Background(), 503, "daniil4545/qualifier")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if got.Title != "qualifier" || got.Context != "Квалификация лидов в Telegram" {
		t.Errorf("фолбэк не сработал: %+v", got)
	}
	if source != "репозиторий" {
		t.Errorf("источник описания: %q", source)
	}
	assertProjectSaved(t, pool, "qualifier", "qualifier")
}

func newTestProjects(t *testing.T, cases *Cases, api, llmProxy string) *Projects {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gh := NewGitHub("token", api, testStatuses, log)
	llm := NewOpenRouter("key", "model", llmProxy, log)
	return NewProjects(cases, gh, llm, "model", log)
}

// cleanupProject убирает проект за тестом: testPool чистит обращения, но не
// таблицу проектов, и мусор утёк бы в соседние тесты.
func cleanupProject(t *testing.T, pool *pgxpool.Pool, slug string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM projects WHERE slug = $1`, slug); err != nil {
			t.Logf("cleanup project %s: %v", slug, err)
		}
	})
}

func assertProjectSaved(t *testing.T, pool *pgxpool.Pool, slug, title string) {
	t.Helper()

	var got string
	err := pool.QueryRow(context.Background(),
		`SELECT title FROM projects WHERE slug = $1 AND active`, slug).Scan(&got)
	if err != nil {
		t.Fatalf("проект %s не сохранён: %v", slug, err)
	}
	if got != title {
		t.Errorf("название проекта: %q, ожидалось %q", got, title)
	}
}

// TestAddProjectKeepsForeignSlug: имя репозитория - ключ проекта, и чужой
// репозиторий с тем же именем не должен перенацеливать существующий. Обращения
// привязаны к проекту по id, и подмена увела бы их тикеты в другой репозиторий.
func TestAddProjectKeepsForeignSlug(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	addProject(t, pool, "planerka")

	server := githubStub(t, map[string]string{
		"GET /repos/other-owner/planerka": `{"name": "planerka", "full_name": "other-owner/planerka",
			"description": "чужой", "private": true}`,
	})
	projects := newTestProjects(t, cases, server.URL, "")

	_, _, err := projects.Add(context.Background(), 504, "other-owner/planerka Чужой | Чужое описание")
	if !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("чужой репозиторий занял ключ: %v", err)
	}

	var owner string
	if err := pool.QueryRow(context.Background(),
		`SELECT github_owner FROM projects WHERE slug = 'planerka'`).Scan(&owner); err != nil {
		t.Fatalf("read project: %v", err)
	}
	if owner != "acme" {
		t.Errorf("владелец репозитория подменён: %q", owner)
	}
}

// TestAddProjectUpdatesSameRepo: тот же репозиторий обновляет свою строку, а не
// заводит вторую, даже если ключ уже отличается от имени репозитория.
func TestAddProjectUpdatesSameRepo(t *testing.T) {
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	cleanupProject(t, pool, "zz-old-slug")

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	seed := ProjectConfig{Slug: "zz-old-slug", Title: "Старое", Owner: "acme",
		Repo: "renamed", Context: "старый контекст"}
	if err := SyncProjects(context.Background(), pool, []ProjectConfig{seed}, log); err != nil {
		t.Fatalf("seed: %v", err)
	}

	server := githubStub(t, map[string]string{
		"GET /repos/acme/renamed": `{"name": "renamed", "full_name": "acme/renamed", "private": true}`,
	})
	projects := newTestProjects(t, cases, server.URL, "")

	got, _, err := projects.Add(context.Background(), 505, "acme/renamed Новое | Новый контекст")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if got.Slug != "zz-old-slug" {
		t.Errorf("ключ пересчитан: %q", got.Slug)
	}
	assertProjectSaved(t, pool, "zz-old-slug", "Новое")

	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM projects WHERE github_owner = 'acme' AND github_repo = 'renamed'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("строк на репозиторий: %d", count)
	}
}
