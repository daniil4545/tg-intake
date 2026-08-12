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

// TestAttemptFitsJob: бюджет попытки обязан быть меньше бюджета работы, иначе
// одна попытка съедает его целиком и повтор внутри работы невозможен вовсе.
func TestAttemptFitsJob(t *testing.T) {
	if llmTimeout >= jobTimeout-llmMinBudget {
		t.Errorf("бюджет попытки %s не укладывается в бюджет работы %s", llmTimeout, jobTimeout)
	}
}
