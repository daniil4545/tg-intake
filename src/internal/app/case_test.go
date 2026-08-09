package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	tele "gopkg.in/telebot.v4"
)

// testPool даёт чистую базу тесту. DSN только из TEST_DATABASE_URL: тест стирает
// данные, а DATABASE_URL молча подхватывается из .env и может смотреть в
// dev-контур.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	_, err = pool.Exec(context.Background(),
		`TRUNCATE cases, case_items, case_events, jobs, users RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// newTestCases: root - корень медиа, у каждого теста свой временный каталог.
func newTestCases(t *testing.T, pool *pgxpool.Pool, root string) *Cases {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	media, err := NewMedia(root, log)
	if err != nil {
		t.Fatalf("new media: %v", err)
	}
	return NewCases(pool, media, log, 30)
}

func insertItem(t *testing.T, pool *pgxpool.Pool, caseID, kind, tgFileID, filePath string) int64 {
	t.Helper()

	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO case_items (case_id, kind, tg_file_id, file_path)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		caseID, kind, nullable(tgFileID), nullable(filePath)).Scan(&id)
	if err != nil {
		t.Fatalf("insert item: %v", err)
	}
	return id
}

func TestBuildProtocol(t *testing.T) {
	items := []Item{
		{ID: 1, Kind: "text", SourceText: "форма не сохраняется", Status: "done"},
		{ID: 2, Kind: "voice", Normalized: "жму сохранить, ничего не происходит", Status: "done"},
		{ID: 3, Kind: "text", SourceText: "у меня то же самое", Status: "done", Forwarded: true},
		{ID: 4, Kind: "photo", Status: "failed", Error: "файл не прочитан"},
	}

	want := "1. текст: форма не сохраняется\n" +
		"2. голосовое: жму сохранить, ничего не происходит\n" +
		"3. текст, переслано (не слова автора): у меня то же самое\n" +
		"4. скриншот: не удалось разобрать: файл не прочитан"

	if got := BuildProtocol(items); got != want {
		t.Errorf("протокол:\nполучено:\n%s\nожидалось:\n%s", got, want)
	}
}

func TestCaseWithoutProject(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	cs, existed, err := cases.StartCase(ctx, User{ID: 5001, First: "Тест"}, "")
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	if existed {
		t.Fatal("первое обращение автора не может быть уже живым")
	}
	if cs.ProjectID != nil {
		t.Fatalf("обращение без выбора проекта: project_id = %v", *cs.ProjectID)
	}

	ask, err := cases.CollectItem(ctx, nil, cs, &tele.Message{ID: 1, Text: "форма не сохраняется"})
	if err != nil {
		t.Fatalf("collect first: %v", err)
	}
	if !ask {
		t.Error("первый элемент без проекта обязан вызвать вопрос о проекте")
	}

	ask, err = cases.CollectItem(ctx, nil, cs, &tele.Message{ID: 2, Text: "и кнопка тоже серая"})
	if err != nil {
		t.Fatalf("collect second: %v", err)
	}
	if ask {
		t.Error("вопрос о проекте задаётся ровно один раз")
	}

	if err := cases.FinishCollect(ctx, cs); !errors.Is(err, ErrNoProject) {
		t.Fatalf("«Готово» без проекта: получено %v, ожидалось ErrNoProject", err)
	}
	if got := reload(t, cases, cs.ID).Status; got != statusCollecting {
		t.Errorf("статус после «Готово» без проекта: %s, ожидался %s", got, statusCollecting)
	}

	if err := cases.SetProject(ctx, cs, "tg-intake"); err != nil {
		t.Fatalf("set project: %v", err)
	}
	if err := cases.FinishCollect(ctx, cs); err != nil {
		t.Fatalf("«Готово» после выбора проекта: %v", err)
	}
	if got := reload(t, cases, cs.ID).Status; got != statusNormalizing {
		t.Errorf("статус после «Готово» с проектом: %s, ожидался %s", got, statusNormalizing)
	}

	items, err := cases.Items(ctx, cs.ID)
	if err != nil {
		t.Fatalf("items: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("сырьё потеряно: элементов %d, ожидалось 2", len(items))
	}
}

// TestCancelWithoutProject: обращение, брошенное до выбора проекта, обязано
// закрываться. До миграции 0005 CHECK разрешал без проекта только collecting,
// отмена падала, и автор не мог ни закрыть обращение, ни пройти мимо.
func TestCancelWithoutProject(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	cs, _, err := cases.StartCase(ctx, User{ID: 5002, First: "Тест"}, "")
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	if _, err := cases.CollectItem(ctx, nil, cs, &tele.Message{ID: 1, Text: "случайная ссылка"}); err != nil {
		t.Fatalf("collect: %v", err)
	}

	if err := cases.CancelCase(ctx, cs, "reset"); err != nil {
		t.Fatalf("отмена без проекта: %v", err)
	}
	if got := reload(t, cases, cs.ID).Status; got != statusCancelled {
		t.Errorf("статус после отмены: %s, ожидался %s", got, statusCancelled)
	}
}

func TestDropFiles(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	root := t.TempDir()
	cases := newTestCases(t, pool, root)

	cs, _, err := cases.StartCase(ctx, User{ID: 5002, First: "Тест"}, "tg-intake")
	if err != nil {
		t.Fatalf("start case: %v", err)
	}

	dir := filepath.Join(root, cs.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create case dir: %v", err)
	}
	for _, name := range []string{"voice", "shot"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		insertItem(t, pool, cs.ID, "photo", "tg-"+name, path)
	}

	if err := cases.DropFiles(ctx, cs.ID); err != nil {
		t.Fatalf("drop files: %v", err)
	}

	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("каталог обращения не удалён: %v", err)
	}
	items, err := cases.Items(ctx, cs.ID)
	if err != nil {
		t.Fatalf("items: %v", err)
	}
	for _, it := range items {
		if it.FilePath != "" {
			t.Errorf("file_path элемента %d не обнулён: %q", it.ID, it.FilePath)
		}
		if it.TgFileID == "" {
			t.Errorf("tg_file_id элемента %d потерян", it.ID)
		}
	}
}

func reload(t *testing.T, cases *Cases, caseID string) *Case {
	t.Helper()

	cs, err := cases.Load(context.Background(), caseID)
	if err != nil {
		t.Fatalf("load case: %v", err)
	}
	if cs == nil {
		t.Fatalf("обращение %s исчезло", caseID)
	}
	return cs
}
