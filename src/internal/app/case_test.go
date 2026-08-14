package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	return NewCases(pool, media, log, 30, 0)
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
		{ID: 5, Kind: "photo", Normalized: "экран заказа 4821", SourceText: "вот тут видно",
			Status: "done", Forwarded: true},
	}

	want := "1. текст: форма не сохраняется\n" +
		"2. голосовое: жму сохранить, ничего не происходит\n" +
		"3. текст, переслано (не слова автора): у меня то же самое\n" +
		"4. скриншот: не удалось разобрать: файл не прочитан\n" +
		"5. скриншот, переслано (не слова автора): экран заказа 4821\n" +
		"   слова автора: вот тут видно"

	if got := BuildProtocol(items); got != want {
		t.Errorf("протокол:\nполучено:\n%s\nожидалось:\n%s", got, want)
	}
}

// TestCollectLinks: адрес обязан дойти до тикета целиком и один раз, откуда бы
// автор его ни прислал - отдельным сообщением или подписью к скриншоту.
func TestCollectLinks(t *testing.T) {
	items := []Item{
		{Kind: "photo", Normalized: "карточка сделки", SourceText: "https://crm.example.com/leads/59630249"},
		{Kind: "text", SourceText: "то же самое видно тут: https://crm.example.com/leads/59630249."},
		{Kind: "voice", Normalized: "задача висит в просрочках", SourceText: "и ещё статус менять надо"},
		{Kind: "link", SourceText: "https://crm.example.com/tasks/12 (там же)"},
	}

	want := []string{"https://crm.example.com/leads/59630249", "https://crm.example.com/tasks/12"}
	got := collectLinks(items)
	if len(got) != len(want) {
		t.Fatalf("ссылок: %d %v, ожидалось %d %v", len(got), got, len(want), want)
	}
	for n := range want {
		if got[n] != want[n] {
			t.Errorf("ссылка %d: %q, ожидалась %q", n, got[n], want[n])
		}
	}
}

func TestCaseWithoutProject(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	cs, existed, err := cases.StartCase(ctx, User{ID: 5001, First: "Тест"}, "", modeTicket)
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

	cs, _, err := cases.StartCase(ctx, User{ID: 5002, First: "Тест"}, "", modeTicket)
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

	cs, _, err := cases.StartCase(ctx, User{ID: 5002, First: "Тест"}, "tg-intake", modeTicket)
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
	}
	// Инвариант «file_id остаётся в БД» проверяется по колонке: в рабочем коде
	// tg_file_id после публикации не читается.
	var lost int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM case_items
		WHERE case_id = $1 AND COALESCE(tg_file_id, '') = ''`, cs.ID).Scan(&lost)
	if err != nil {
		t.Fatalf("count tg_file_id: %v", err)
	}
	if lost > 0 {
		t.Errorf("tg_file_id потерян у %d элементов", lost)
	}
}

// TestEndAskFreesSlot: «Закончить разговор» закрывает вопрос полученным
// ответом, и слот активного обращения обязан освободиться - иначе следующий
// вопрос автору некуда завести.
func TestEndAskFreesSlot(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	author := User{ID: 5006, First: "Тест"}
	cs, _, err := cases.StartCase(ctx, author, "tg-intake", modeAsk)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	if _, err := cases.CollectItem(ctx, nil, cs, &tele.Message{ID: 1, Text: "через сколько срабатывает опрос"}); err != nil {
		t.Fatalf("collect: %v", err)
	}

	if err := cases.EndAsk(ctx, cs); err != nil {
		t.Fatalf("end ask: %v", err)
	}
	if got := reload(t, cases, cs.ID).Status; got != statusAnswered {
		t.Errorf("статус после «Закончить разговор»: %s, ожидался %s", got, statusAnswered)
	}

	active, err := cases.Active(ctx, author.ID)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if active != nil {
		t.Fatalf("слот держит закрытый разговор %s", active.ID)
	}

	next, existed, err := cases.StartCase(ctx, author, "tg-intake", modeTicket)
	if err != nil {
		t.Fatalf("start next case: %v", err)
	}
	if existed || next.ID == cs.ID {
		t.Error("следующее обращение не завелось: закрытый разговор считается живым")
	}
}

// TestRecoverStuckLookup: поход в документацию идёт своим статусом именно
// потому, что потерянную работу иначе некому поднять - в collecting обращение
// ждёт человека, а в answering оно висело бы молча.
func TestRecoverStuckLookup(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	cs, _, err := cases.StartCase(ctx, User{ID: 5007, First: "Тест"}, "tg-intake", modeAsk)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'answering' WHERE id = $1`, cs.ID); err != nil {
		t.Fatalf("set answering: %v", err)
	}

	if err := cases.RecoverStuck(ctx); err != nil {
		t.Fatalf("recover stuck: %v", err)
	}
	if jobID(t, pool, JobLookup+":"+cs.ID) == 0 {
		t.Error("потерянный поход в документацию не поднят")
	}
}

// TestSwitchDuringNormalize: «Создать тикет», нажатая во время разбора, разговор
// не переводит. Иначе цепочка нормализации молча выходит на чужом статусе, и
// материал последней реплики остаётся неразобранным навсегда.
func TestSwitchDuringNormalize(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	normalizer := NewNormalizer(cases, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cs, _, err := cases.StartCase(ctx, User{ID: 5008, First: "Тест"}, "tg-intake", modeAsk)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	if _, err := cases.CollectItem(ctx, nil, cs, &tele.Message{ID: 1, Text: "а можно раз в двенадцать часов"}); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if err := cases.FinishCollect(ctx, cs); err != nil {
		t.Fatalf("finish collect: %v", err)
	}

	switch switched, err := cases.SwitchToTicket(ctx, cs); {
	case err != nil:
		t.Fatalf("switch to ticket: %v", err)
	case switched:
		t.Fatal("перевод из разбора объявлен состоявшимся")
	}
	if got := reload(t, cases, cs.ID).Status; got != statusNormalizing {
		t.Fatalf("статус после перевода из разбора: %s, ожидался %s", got, statusNormalizing)
	}

	runChain(t, normalizer, pool, cs.ID)
	done := reload(t, cases, cs.ID)
	if done.Status != statusAnswering {
		t.Errorf("статус после разбора: %s, ожидался %s", done.Status, statusAnswering)
	}
	if !strings.Contains(done.Protocol, "двенадцать часов") {
		t.Errorf("сырьё потеряно, протокол: %q", done.Protocol)
	}
}

// TestSwitchKeepsUnfinishedRaw: реплика, набранная после ответа и не
// подтверждённая «Готово», - это и есть содержание будущего тикета. Переход
// кнопкой обязан довести её до протокола и до интервью, а не потерять молча.
func TestSwitchKeepsUnfinishedRaw(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	normalizer := NewNormalizer(cases, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cs, _, err := cases.StartCase(ctx, User{ID: 5010, First: "Тест"}, "tg-intake", modeAsk)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	if _, err := cases.CollectItem(ctx, nil, cs, &tele.Message{ID: 1, Text: "через сколько срабатывает опрос"}); err != nil {
		t.Fatalf("collect question: %v", err)
	}
	if err := cases.FinishCollect(ctx, cs); err != nil {
		t.Fatalf("finish collect: %v", err)
	}
	runChain(t, normalizer, pool, cs.ID)

	// Ответ показан: так разговор возвращает шаг похода в документацию. Реплику
	// автор дописывает и жмёт «Создать тикет», не подтверждая её «Готово».
	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'collecting' WHERE id = $1`, cs.ID); err != nil {
		t.Fatalf("return to collecting: %v", err)
	}
	cs = reload(t, cases, cs.ID)
	if _, err := cases.CollectItem(ctx, nil, cs, &tele.Message{ID: 2, Text: "давай изменим на 12 часов"}); err != nil {
		t.Fatalf("collect reply: %v", err)
	}
	if _, err := cases.SwitchToTicket(ctx, cs); err != nil {
		t.Fatalf("switch to ticket: %v", err)
	}
	runChain(t, normalizer, pool, cs.ID)

	done := reload(t, cases, cs.ID)
	if done.Mode != modeTicket || done.Status != statusInterview {
		t.Errorf("режим %s, статус %s: ожидались %s и %s",
			done.Mode, done.Status, modeTicket, statusInterview)
	}
	if !strings.Contains(done.Protocol, "12 часов") {
		t.Errorf("реплика не дошла до протокола: %q", done.Protocol)
	}
	if jobID(t, pool, JobInterview+":"+cs.ID) == 0 {
		t.Error("ход интервью не поставлен")
	}
}

// TestRepeatedDoneInAsk: после показанного ответа разговор снова стоит в сборе с
// прежним сырьём. Повторное «Готово» без нового вопроса не имеет права поставить
// второй поход в документацию за тем же ответом.
func TestRepeatedDoneInAsk(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	normalizer := NewNormalizer(cases, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cs, _, err := cases.StartCase(ctx, User{ID: 5009, First: "Тест"}, "tg-intake", modeAsk)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	if _, err := cases.CollectItem(ctx, nil, cs, &tele.Message{ID: 1, Text: "через сколько срабатывает опрос"}); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if err := cases.FinishCollect(ctx, cs); err != nil {
		t.Fatalf("finish collect: %v", err)
	}
	runChain(t, normalizer, pool, cs.ID)

	// Ответ показан: так разговор возвращает шаг похода в документацию.
	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'collecting' WHERE id = $1`, cs.ID); err != nil {
		t.Fatalf("return to collecting: %v", err)
	}
	again := reload(t, cases, cs.ID)
	if err := cases.FinishCollect(ctx, again); !errors.Is(err, ErrNoNewItems) {
		t.Fatalf("повторное «Готово»: получено %v, ожидалось ErrNoNewItems", err)
	}
	if got := reload(t, cases, cs.ID).Status; got != statusCollecting {
		t.Errorf("статус после повторного «Готово»: %s, ожидался %s", got, statusCollecting)
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
