package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	tele "gopkg.in/telebot.v4"
)

func TestScreenshotValidate(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{
			"без поля unreadable",
			`{"screen":"карточка заказа","facts":[{"label":"статус","value":"новый"}],"relevant":"та самая карточка"}`,
			false,
		},
		{
			"пустые facts и непустой unreadable",
			`{"screen":"карточка заказа","facts":[],"relevant":"","unreadable":["нижняя часть экрана"]}`,
			true,
		},
		{
			"валидный разбор",
			`{"screen":"карточка заказа","facts":[{"label":"статус","value":"новый"}],"relevant":"та самая карточка","unreadable":[]}`,
			true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseExtract(json.RawMessage(c.raw))
			if c.ok && err != nil {
				t.Errorf("ответ отклонён: %v", err)
			}
			if !c.ok && err == nil {
				t.Error("ответ принят, хотя обязан быть отклонён")
			}
		})
	}
}

// TestFailedItemMovesOn: голосовое, чья работа исчерпала повторы, не имеет права
// держать обращение в normalizing. Второе голосовое к этому моменту разобрано,
// поэтому цепочку двигает именно исход провала.
func TestFailedItemMovesOn(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())

	cs, _, err := cases.StartCase(ctx, User{ID: 5003, First: "Тест"}, "tg-intake", modeTicket)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	broken := insertItem(t, pool, cs.ID, "voice", "tg-broken", "")
	live := insertItem(t, pool, cs.ID, "voice", "tg-live", "")

	if err := cases.FinishCollect(ctx, cs); err != nil {
		t.Fatalf("finish collect: %v", err)
	}
	finishKey := JobFinishNormalize + ":" + cs.ID
	if jobID(t, pool, finishKey) != 0 {
		t.Fatal("finish_normalize поставлен, пока голосовые не разобраны")
	}

	_, err = pool.Exec(ctx, `
		UPDATE case_items SET normalized = 'жму сохранить, ничего не происходит',
		                      status = 'done' WHERE id = $1`, live)
	if err != nil {
		t.Fatalf("normalize live item: %v", err)
	}
	if err := cases.AdvanceNormalize(ctx, cs.ID); err != nil {
		t.Fatalf("advance normalize: %v", err)
	}
	if jobID(t, pool, finishKey) != 0 {
		t.Fatal("finish_normalize поставлен, пока битое голосовое ещё pending")
	}

	voiceKey := fmt.Sprintf("%s:%s:%d", JobNormalizeVoice, cs.ID, broken)
	voiceJob := Job{
		ID:       jobID(t, pool, voiceKey),
		Kind:     JobNormalizeVoice,
		Key:      voiceKey,
		Payload:  json.RawMessage(fmt.Sprintf(`{"case_id":%q,"item_id":%d}`, cs.ID, broken)),
		Attempts: maxAttempts + 1,
	}
	cause := errors.New("openrouter unreachable")
	dead, err := FailJob(ctx, pool, voiceJob, cause)
	if err != nil {
		t.Fatalf("fail job: %v", err)
	}
	if !dead {
		t.Fatalf("работа с attempts=%d обязана быть исчерпана", voiceJob.Attempts)
	}
	cases.HandleFailedJob(ctx, voiceJob, cause)

	if got := itemStatus(t, pool, broken); got != "failed" {
		t.Errorf("статус битого элемента: %s, ожидался failed", got)
	}
	finishID := jobID(t, pool, finishKey)
	if finishID == 0 {
		t.Fatal("finish_normalize не поставлен: цепочка встала на провале голосового")
	}

	normalizer := NewNormalizer(cases, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	finishJob := Job{
		ID:      finishID,
		Kind:    JobFinishNormalize,
		Key:     finishKey,
		Payload: json.RawMessage(fmt.Sprintf(`{"case_id":%q}`, cs.ID)),
	}
	if err := normalizer.RunFinishNormalize(ctx, finishJob); err != nil {
		t.Fatalf("run finish normalize: %v", err)
	}

	done := reload(t, cases, cs.ID)
	if done.Status == statusNormalizing {
		t.Error("обращение осталось в normalizing")
	}
	if !strings.Contains(done.Protocol, "не удалось разобрать") {
		t.Errorf("провал не виден в протоколе:\n%s", done.Protocol)
	}
}

// TestReopenedCaseStartsSecondRound: обращение, вернувшееся в сбор, обязано
// запускаться повторно. Ключи работ детерминированы, поэтому погашенная работа
// прошлого захода съедала бы новую через ON CONFLICT DO NOTHING, и второе
// «Готово» вешало обращение в normalizing навсегда.
func TestReopenedCaseStartsSecondRound(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	normalizer := NewNormalizer(cases, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cs, _, err := cases.StartCase(ctx, User{ID: 5004, First: "Тест"}, "tg-intake", modeTicket)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	// Скриншот без файла: разбирать нечего, весь материал провалится, и работа
	// вернёт обращение в сбор.
	insertItem(t, pool, cs.ID, "photo", "tg-shot", "")

	if err := cases.FinishCollect(ctx, cs); err != nil {
		t.Fatalf("finish collect: %v", err)
	}
	runChain(t, normalizer, pool, cs.ID)

	cs = reload(t, cases, cs.ID)
	if cs.Status != statusCollecting {
		t.Fatalf("статус после возврата в сбор: %s, ожидался %s", cs.Status, statusCollecting)
	}

	if _, err := cases.CollectItem(ctx, nil, cs, &tele.Message{ID: 2, Text: "форма не сохраняется"}); err != nil {
		t.Fatalf("collect after reopen: %v", err)
	}
	if err := cases.FinishCollect(ctx, cs); err != nil {
		t.Fatalf("второе «Готово»: %v", err)
	}

	// Скриншот первого захода погашен, разбирать во втором нечего: цепочка идёт
	// сразу к закрытию нормализации, и её работа обязана быть новой.
	secondJob := jobID(t, pool, JobFinishNormalize+":"+cs.ID)
	if secondJob == 0 || jobStatus(t, pool, secondJob) != "pending" {
		t.Fatalf("второе «Готово» не поставило работу: id %d", secondJob)
	}
	runChain(t, normalizer, pool, cs.ID)

	done := reload(t, cases, cs.ID)
	if done.Status != "interview" {
		t.Errorf("статус после второго захода: %s, ожидался interview", done.Status)
	}
	if !strings.Contains(done.Protocol, "форма не сохраняется") {
		t.Errorf("материал второго захода не попал в протокол:\n%s", done.Protocol)
	}
}

// TestScreenshotJobPerItem: каждый скриншот получает свою работу, и провал
// одного разбора не мешает следующему дойти до конца. Пачкой в одной работе они
// делили бюджет: первый же зависший экран уводил в повтор всю нормализацию.
func TestScreenshotJobPerItem(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	normalizer := NewNormalizer(cases, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cs, _, err := cases.StartCase(ctx, User{ID: 5005, First: "Тест"}, "tg-intake", modeTicket)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	if _, err := cases.CollectItem(ctx, nil, cs, &tele.Message{ID: 1, Text: "заявка не сохраняется"}); err != nil {
		t.Fatalf("collect text: %v", err)
	}
	// Оба без файла: разбор гаснет до вызова модели, а проверяется здесь не он,
	// а то, что работы идут по элементу и цепочка доходит до конца.
	first := insertItem(t, pool, cs.ID, "photo", "tg-shot-1", "")
	second := insertItem(t, pool, cs.ID, "photo", "tg-shot-2", "")

	if err := cases.FinishCollect(ctx, cs); err != nil {
		t.Fatalf("finish collect: %v", err)
	}
	firstKey := fmt.Sprintf("%s:%s:%d", JobNormalizeImage, cs.ID, first)
	secondKey := fmt.Sprintf("%s:%s:%d", JobNormalizeImage, cs.ID, second)
	firstJob := jobID(t, pool, firstKey)
	if firstJob == 0 || jobID(t, pool, secondKey) == 0 {
		t.Fatal("скриншоты не получили по своей работе")
	}
	if jobID(t, pool, JobFinishNormalize+":"+cs.ID) != 0 {
		t.Fatal("finish_normalize поставлен, пока скриншоты не разобраны")
	}

	err = normalizer.RunNormalizeImage(ctx, Job{
		ID:      firstJob,
		Kind:    JobNormalizeImage,
		Key:     firstKey,
		Payload: json.RawMessage(fmt.Sprintf(`{"case_id":%q,"item_id":%d}`, cs.ID, first)),
	})
	if err != nil {
		t.Fatalf("run normalize image: %v", err)
	}
	if got := itemStatus(t, pool, first); got != "failed" {
		t.Errorf("статус первого скриншота: %s, ожидался failed", got)
	}
	if jobID(t, pool, secondKey) == 0 {
		t.Fatal("работа второго скриншота снята провалом первого")
	}
	if jobID(t, pool, JobFinishNormalize+":"+cs.ID) != 0 {
		t.Fatal("finish_normalize поставлен, пока второй скриншот ещё pending")
	}

	runChain(t, normalizer, pool, cs.ID)

	done := reload(t, cases, cs.ID)
	if done.Status != statusInterview {
		t.Fatalf("статус после разбора: %s, ожидался %s", done.Status, statusInterview)
	}
	if !strings.Contains(done.Protocol, "заявка не сохраняется") {
		t.Errorf("текст обращения не попал в протокол:\n%s", done.Protocol)
	}
}

// TestAskModeGoesToLookup: режим разговора выбирает следующий шаг, и вопрос
// уходит в документацию, а не в интервью тикета.
func TestAskModeGoesToLookup(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	normalizer := NewNormalizer(cases, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cs, _, err := cases.StartCase(ctx, User{ID: 5006, First: "Тест"}, "tg-intake", modeAsk)
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

	if got := reload(t, cases, cs.ID).Status; got != statusAnswering {
		t.Errorf("статус после разбора вопроса: %s, ожидался %s", got, statusAnswering)
	}
	if jobID(t, pool, JobLookup+":"+cs.ID) == 0 {
		t.Error("поход в документацию не поставлен")
	}
	if jobID(t, pool, JobInterview+":"+cs.ID) != 0 {
		t.Error("вопрос ушёл в интервью тикета")
	}
}

// TestQuestionDeltaSkipsPrevious: в историю разговора идёт только новая часть
// протокола. Иначе прошлые вопросы придут в контекст модели дважды.
func TestQuestionDeltaSkipsPrevious(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	normalizer := NewNormalizer(cases, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cs, _, err := cases.StartCase(ctx, User{ID: 5007, First: "Тест"}, "tg-intake", modeAsk)
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	ask := func(id int, text string) {
		t.Helper()
		fresh := reload(t, cases, cs.ID)
		if _, err := cases.CollectItem(ctx, nil, fresh, &tele.Message{ID: id, Text: text}); err != nil {
			t.Fatalf("collect %q: %v", text, err)
		}
		if err := cases.FinishCollect(ctx, fresh); err != nil {
			t.Fatalf("finish collect %q: %v", text, err)
		}
		runChain(t, normalizer, pool, cs.ID)
	}

	ask(1, "через сколько срабатывает опрос")
	// Ответ показан, разговор снова ждёт реплики автора: так его возвращает шаг
	// похода в документацию.
	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'collecting' WHERE id = $1`, cs.ID); err != nil {
		t.Fatalf("return to collecting: %v", err)
	}
	ask(2, "а можно раз в двенадцать часов")

	last := lastQuestion(t, pool, cs.ID)
	if !strings.Contains(last, "двенадцать часов") {
		t.Errorf("новый вопрос не попал в историю: %q", last)
	}
	if strings.Contains(last, "срабатывает опрос") {
		t.Errorf("прошлый вопрос повторён в истории: %q", last)
	}
}

// lastQuestion - текст последней реплики автора в журнале разговора.
func lastQuestion(t *testing.T, pool *pgxpool.Pool, caseID string) string {
	t.Helper()

	var text string
	err := pool.QueryRow(context.Background(), `
		SELECT payload->>'text' FROM case_events
		WHERE case_id = $1 AND kind = 'question_asked' ORDER BY id DESC LIMIT 1`, caseID).Scan(&text)
	if err != nil {
		t.Fatalf("last question of case %s: %v", caseID, err)
	}
	return text
}

// runChain доигрывает цепочку нормализации так, как её выполнял бы воркер:
// работа по элементу, следом закрывающая. Расшифровка сюда не попадает - ей
// нужна модель, и её проверяют отдельно.
func runChain(t *testing.T, n *Normalizer, pool *pgxpool.Pool, caseID string) {
	t.Helper()
	ctx := context.Background()

	for range 10 {
		job, ok := nextJob(t, pool, caseID)
		if !ok {
			return
		}
		var err error
		switch job.Kind {
		case JobNormalizeImage:
			err = n.RunNormalizeImage(ctx, job)
		default:
			err = n.RunFinishNormalize(ctx, job)
		}
		if err != nil {
			t.Fatalf("run %s: %v", job.Kind, err)
		}
		if _, err := FinishJob(ctx, pool, job.ID); err != nil {
			t.Fatalf("finish %s: %v", job.Kind, err)
		}
	}
	t.Fatal("цепочка нормализации не сошлась за десять шагов")
}

func nextJob(t *testing.T, pool *pgxpool.Pool, caseID string) (Job, bool) {
	t.Helper()

	var job Job
	// Только работы нормализации: уведомления автору в этих тестах доставлять
	// некому, и они остаются в очереди как есть.
	err := pool.QueryRow(context.Background(), `
		SELECT id, kind, key, payload FROM jobs
		WHERE status = 'pending' AND payload->>'case_id' = $1
		  AND kind IN ($2, $3)
		ORDER BY id LIMIT 1`, caseID, JobNormalizeImage, JobFinishNormalize).
		Scan(&job.ID, &job.Kind, &job.Key, &job.Payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false
	}
	if err != nil {
		t.Fatalf("next job of case %s: %v", caseID, err)
	}
	return job, true
}

// jobID отдаёт 0, если работы с таким ключом нет: «не поставлена» - такой же
// ожидаемый исход, как «поставлена».
func jobID(t *testing.T, pool *pgxpool.Pool, key string) int64 {
	t.Helper()

	var id int64
	err := pool.QueryRow(context.Background(),
		`SELECT id FROM jobs WHERE key = $1`, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("find job %s: %v", key, err)
	}
	return id
}

func jobStatus(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()

	var status string
	err := pool.QueryRow(context.Background(),
		`SELECT status FROM jobs WHERE id = $1`, id).Scan(&status)
	if err != nil {
		t.Fatalf("job %d status: %v", id, err)
	}
	return status
}

func itemStatus(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()

	var status string
	err := pool.QueryRow(context.Background(),
		`SELECT status FROM case_items WHERE id = $1`, id).Scan(&status)
	if err != nil {
		t.Fatalf("item %d status: %v", id, err)
	}
	return status
}
