package app

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"testing"
)

func TestAuthorSlug(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

	cases := []struct {
		name string
		user User
		want string
	}{
		{"кириллица без username", User{ID: 1, First: "Иван", Last: "Петров"}, "ivan-petrov"},
		{"username с заглавными", User{ID: 2, First: "Иван", Username: "Ivan_Petrov"}, "ivan-petrov"},
		{"имя без пригодных символов", User{ID: 42, First: "🙂"}, "user-42"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := authorSlug(c.user)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
			if !valid.MatchString(got) {
				t.Errorf("slug %q is not a valid label value", got)
			}
		})
	}
}

// TestSyncProjects: список проектов приходит из конфига контура и приводит
// таблицу к себе. Неперечисленный проект не трогается: опечатка в переменной не
// должна гасить живой проект вместе с его тикетами.
func TestSyncProjects(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	off := false

	addProject(t, pool, "zz-sync")
	seeded := ProjectConfig{Slug: "zz-sync", Title: "Переименован", Owner: "acme",
		Repo: "other-repo", Context: "новый контекст"}
	if err := SyncProjects(ctx, pool, []ProjectConfig{seeded}, log); err != nil {
		t.Fatalf("seed existing: %v", err)
	}

	var title, repo string
	err := pool.QueryRow(ctx, `SELECT title, github_repo FROM projects WHERE slug = 'zz-sync'`).
		Scan(&title, &repo)
	if err != nil {
		t.Fatalf("read project: %v", err)
	}
	// Сид не трогает существующую строку: иначе рестарт откатывал бы всё, что
	// автор поправил командой.
	if title == "Переименован" || repo == "other-repo" {
		t.Errorf("сид перезаписал живой проект: %q %q", title, repo)
	}

	// Проект seed в списке не перечислен и остаться должен как был.
	if p := testProject(t, pool); p.Slug != "tg-intake" {
		t.Errorf("неперечисленный проект пропал: %+v", p)
	}

	cleanupProject(t, pool, "zz-new")
	fresh := ProjectConfig{Slug: "zz-new", Title: "Новый", Owner: "acme",
		Repo: "fresh", Context: "контекст", Active: &off}
	if err := SyncProjects(ctx, pool, []ProjectConfig{fresh}, log); err != nil {
		t.Fatalf("seed new: %v", err)
	}
	projects, err := ListProjects(ctx, pool)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	for _, p := range projects {
		if p.Slug == "zz-new" {
			t.Error("проект с active: false попал в меню")
		}
	}
}
