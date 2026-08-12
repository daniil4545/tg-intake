package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// TestRetryNeedsBudget: повтор генерирует ответ заново и стоит как первая
// попытка. На исходе дедлайна работы он не успеет, поэтому не начинается вовсе -
// иначе токены списываются за работу, которую оборвут на середине.
func TestRetryNeedsBudget(t *testing.T) {
	calls := 0
	llm := NewOpenRouter("key", "model", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	llm.http = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		// 5xx повторяют: без проверки бюджета клиент пошёл бы на второй круг.
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	})}

	ctx, cancel := context.WithTimeout(context.Background(), llmMinBudget/2)
	defer cancel()

	_, err := llm.Complete(ctx, Request{
		Step:     "interview",
		Messages: []Message{{Role: "user", Parts: []Part{TextPart("привет")}}},
		Schema:   []byte(`{"type":"object"}`),
	})
	if err == nil {
		t.Fatal("вызов без бюджета обязан вернуть ошибку")
	}
	if !strings.Contains(err.Error(), "no budget") {
		t.Errorf("ошибка не называет причину: %v", err)
	}
	if calls != 1 {
		t.Errorf("попыток: %d, ожидалась 1", calls)
	}
}

// TestRetryRunsWithBudget: обратная сторона проверки бюджета - при живом
// дедлайне повтор идёт и доводит вызов до ответа.
func TestRetryRunsWithBudget(t *testing.T) {
	calls := 0
	llm := NewOpenRouter("key", "model", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	llm.http = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: http.StatusBadGateway,
				Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(
			`{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`))}, nil
	})}

	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()

	out, err := llm.Complete(ctx, Request{
		Step:     "interview",
		Messages: []Message{{Role: "user", Parts: []Part{TextPart("привет")}}},
		Schema:   []byte(`{"type":"object"}`),
	})
	if err != nil {
		t.Fatalf("повтор с живым бюджетом не дошёл до ответа: %v", err)
	}
	if calls != 2 {
		t.Errorf("попыток: %d, ожидалось 2", calls)
	}
	if !strings.Contains(string(out), "ok") {
		t.Errorf("ответ не дошёл: %s", out)
	}
}

// TestRequestLimitsGeneration: размышления модели считаются выходными токенами
// и занимают больше времени, чем сам ответ. Уровень уходит в запрос только
// заданным: требовать поддержку reasoning у провайдера, которому она не нужна,
// значит сузить выбор без причины.
func TestRequestLimitsGeneration(t *testing.T) {
	var body string
	llm := NewOpenRouter("key", "model", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	llm.http = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(
			`{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`))}, nil
	})}

	req := Request{
		Step:     "interview",
		Messages: []Message{{Role: "user", Parts: []Part{TextPart("привет")}}},
		Schema:   []byte(`{"type":"object"}`),
	}
	if _, err := llm.Complete(context.Background(), req); err != nil {
		t.Fatalf("вызов без уровня размышлений: %v", err)
	}
	if strings.Contains(body, "reasoning") {
		t.Errorf("пустой уровень ушёл в запрос: %s", body)
	}
	if !strings.Contains(body, `"max_tokens"`) {
		t.Errorf("в запросе нет потолка ответа: %s", body)
	}

	req.Reasoning = "low"
	if _, err := llm.Complete(context.Background(), req); err != nil {
		t.Fatalf("вызов с уровнем размышлений: %v", err)
	}
	if !strings.Contains(body, `"reasoning":{"effort":"low"}`) {
		t.Errorf("уровень размышлений не ушёл в запрос: %s", body)
	}
}

// TestRetryFitsJob: в бюджет работы обязаны укладываться попытка и повтор -
// иначе проверка остатка гасит вторую попытку всегда, и повторов нет вовсе.
func TestRetryFitsJob(t *testing.T) {
	if llmTimeout+llmMinBudget > jobTimeout {
		t.Errorf("попытка %s плюс порог повтора %s не влезают в бюджет работы %s",
			llmTimeout, llmMinBudget, jobTimeout)
	}
}
