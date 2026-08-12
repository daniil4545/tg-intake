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

	cs, _, err := cases.StartCase(ctx, User{ID: 5003, First: "Тест"}, "tg-intake")
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	broken := insertItem(t, pool, cs.ID, "voice", "tg-broken", "")
	live := insertItem(t, pool, cs.ID, "voice", "tg-live", "")

	if err := cases.FinishCollect(ctx, cs); err != nil {
		t.Fatalf("finish collect: %v", err)
	}
	imagesKey := JobNormalizeImages + ":" + cs.ID
	if jobID(t, pool, imagesKey) != 0 {
		t.Fatal("normalize_images поставлен, пока голосовые не разобраны")
	}

	_, err = pool.Exec(ctx, `
		UPDATE case_items SET normalized = 'жму сохранить, ничего не происходит',
		                      status = 'done' WHERE id = $1`, live)
	if err != nil {
		t.Fatalf("normalize live item: %v", err)
	}
	if err := cases.PutImagesJob(ctx, cs.ID); err != nil {
		t.Fatalf("put images job: %v", err)
	}
	if jobID(t, pool, imagesKey) != 0 {
		t.Fatal("normalize_images поставлен, пока битое голосовое ещё pending")
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
	imagesID := jobID(t, pool, imagesKey)
	if imagesID == 0 {
		t.Fatal("normalize_images не поставлен: цепочка встала на провале голосового")
	}

	normalizer := NewNormalizer(cases, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	imagesJob := Job{
		ID:      imagesID,
		Kind:    JobNormalizeImages,
		Key:     imagesKey,
		Payload: json.RawMessage(fmt.Sprintf(`{"case_id":%q}`, cs.ID)),
	}
	if err := normalizer.RunNormalizeImages(ctx, imagesJob); err != nil {
		t.Fatalf("run normalize images: %v", err)
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

	cs, _, err := cases.StartCase(ctx, User{ID: 5004, First: "Тест"}, "tg-intake")
	if err != nil {
		t.Fatalf("start case: %v", err)
	}
	// Скриншот без файла: разбирать нечего, весь материал провалится, и работа
	// вернёт обращение в сбор.
	insertItem(t, pool, cs.ID, "photo", "tg-shot", "")

	if err := cases.FinishCollect(ctx, cs); err != nil {
		t.Fatalf("finish collect: %v", err)
	}
	imagesKey := JobNormalizeImages + ":" + cs.ID
	firstJob := jobID(t, pool, imagesKey)
	if firstJob == 0 {
		t.Fatal("normalize_images не поставлен на первом «Готово»")
	}
	err = normalizer.RunNormalizeImages(ctx, Job{
		ID:      firstJob,
		Kind:    JobNormalizeImages,
		Key:     imagesKey,
		Payload: json.RawMessage(fmt.Sprintf(`{"case_id":%q}`, cs.ID)),
	})
	if err != nil {
		t.Fatalf("run normalize images: %v", err)
	}
	// Воркер гасит успешно вернувшую nil работу как done.
	if err := FinishJob(ctx, pool, firstJob); err != nil {
		t.Fatalf("finish job: %v", err)
	}

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

	secondJob := jobID(t, pool, imagesKey)
	if secondJob == 0 || jobStatus(t, pool, secondJob) != "pending" {
		t.Fatalf("второе «Готово» не поставило работу: id %d", secondJob)
	}

	err = normalizer.RunNormalizeImages(ctx, Job{
		ID:      secondJob,
		Kind:    JobNormalizeImages,
		Key:     imagesKey,
		Payload: json.RawMessage(fmt.Sprintf(`{"case_id":%q}`, cs.ID)),
	})
	if err != nil {
		t.Fatalf("run normalize images again: %v", err)
	}

	done := reload(t, cases, cs.ID)
	if done.Status != "interview" {
		t.Errorf("статус после второго захода: %s, ожидался interview", done.Status)
	}
	if !strings.Contains(done.Protocol, "форма не сохраняется") {
		t.Errorf("материал второго захода не попал в протокол:\n%s", done.Protocol)
	}
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
