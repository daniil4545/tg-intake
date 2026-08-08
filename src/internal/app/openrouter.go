package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	openRouterURL = "https://openrouter.ai/api/v1/chat/completions"
	llmTimeout    = 60 * time.Second
	llmRetries    = 3
	llmBodyLimit  = 1 << 20
)

// OpenRouter - клиент chat/completions. Один эндпоинт не оправдывает SDK:
// чужая модель ошибок и повторов дороже своих ста строк.
type OpenRouter struct {
	key   string
	model string
	http  *http.Client
	log   *slog.Logger
}

func NewOpenRouter(key, model string, log *slog.Logger) *OpenRouter {
	// Таймаут на попытку, а не на вызов целиком: общий предел задаёт ctx
	// вызывающего, иначе три повтора незаметно растянулись бы на четыре минуты
	// внутри одного дедлайна.
	return &OpenRouter{key: key, model: model, http: &http.Client{Timeout: llmTimeout}, log: log}
}

// Request - один вызов модели.
//
// Порядок Messages сохраняется как есть, клиент ничего не вставляет между
// сообщениями: стабильный префикс обязан идти первым сообщением, и любая
// изменяющаяся строка перед ним молча гасит кэш провайдера.
type Request struct {
	Step       string          // имя шага для лога; содержимое запроса не логируется
	Model      string          // пусто - модель клиента по умолчанию
	Messages   []Message       // префикс первым, волатильное последним
	SchemaName string          // имя схемы в response_format, пусто - "response"
	Schema     json.RawMessage // тело JSON Schema ответа
}

type Message struct {
	Role  string
	Parts []Part
}

// Part - кусок содержимого сообщения. Поля закрыты: вид куска задаётся
// конструктором, чтобы не собралось сообщение с аудио и mime картинки разом.
type Part struct {
	kind   string
	text   string
	data   []byte
	format string
}

func TextPart(text string) Part { return Part{kind: "text", text: text} }

// AudioPart принимает только байты: URL в input_audio OpenRouter не берёт.
// format - контейнер записи (ogg, mp3), он же подсказка провайдеру о кодеке.
func AudioPart(data []byte, format string) Part {
	return Part{kind: "audio", data: data, format: format}
}

// ImagePart уходит как data:<mime>;base64 в image_url.
func ImagePart(data []byte, mime string) Part {
	return Part{kind: "image", data: data, format: mime}
}

func (m Message) MarshalJSON() ([]byte, error) {
	wire := struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}{Role: m.Role}

	// Единственный текстовый кусок отдаём строкой: массив content-частей в
	// system-сообщении принимают не все провайдеры.
	if len(m.Parts) == 1 && m.Parts[0].kind == "text" {
		wire.Content = m.Parts[0].text
	} else {
		wire.Content = m.Parts
	}
	return json.Marshal(wire)
}

func (p Part) MarshalJSON() ([]byte, error) {
	switch p.kind {
	case "text":
		return json.Marshal(map[string]string{"type": "text", "text": p.text})
	case "audio":
		return json.Marshal(map[string]any{
			"type": "input_audio",
			"input_audio": map[string]string{
				"data":   base64.StdEncoding.EncodeToString(p.data),
				"format": p.format,
			},
		})
	case "image":
		return json.Marshal(map[string]any{
			"type": "image_url",
			"image_url": map[string]string{
				"url": "data:" + p.format + ";base64," + base64.StdEncoding.EncodeToString(p.data),
			},
		})
	default:
		return nil, fmt.Errorf("unknown content part %q", p.kind)
	}
}

type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []Message      `json:"messages"`
	ResponseFormat responseFormat `json:"response_format"`
	Provider       providerOpts   `json:"provider"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

// providerOpts: без require_parameters запрос уедет к провайдеру без поддержки
// схемы и вернётся свободным текстом вместо структуры.
type providerOpts struct {
	RequireParameters bool `json:"require_parameters"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type llmResult struct {
	content   json.RawMessage
	tokensIn  int
	tokensOut int
}

// Complete возвращает content первого choice сырым: в схему его разбирает
// вызывающий, он же и валидирует - вывод модели недоверенный.
func (c *OpenRouter) Complete(ctx context.Context, req Request) (json.RawMessage, error) {
	if len(req.Messages) == 0 || len(req.Schema) == 0 {
		return nil, errors.New("openrouter: request needs messages and schema")
	}

	model := req.Model
	if model == "" {
		model = c.model
	}
	name := req.SchemaName
	if name == "" {
		name = "response"
	}

	body, err := json.Marshal(chatRequest{
		Model:          model,
		Messages:       req.Messages,
		ResponseFormat: responseFormat{Type: "json_schema", JSONSchema: jsonSchema{Name: name, Strict: true, Schema: req.Schema}},
		Provider:       providerOpts{RequireParameters: true},
	})
	if err != nil {
		return nil, fmt.Errorf("openrouter %s: build request: %w", req.Step, err)
	}

	start := time.Now()
	for attempt := 0; ; attempt++ {
		out, retry, err := c.send(ctx, body)
		if err == nil {
			c.log.Info("llm_call",
				"step", req.Step,
				"model", model,
				"ms", time.Since(start).Milliseconds(),
				"tokens_in", out.tokensIn,
				"tokens_out", out.tokensOut)
			return out.content, nil
		}
		if !retry || attempt == llmRetries {
			return nil, fmt.Errorf("openrouter %s: %w", req.Step, err)
		}

		c.log.Warn("llm_retry", "step", req.Step, "model", model, "attempt", attempt+1, "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * time.Second):
		}
	}
}

// send делает одну попытку. Второе значение - повторять ли: 429 и 5xx да, 4xx
// нет, разбитое соединение да - ответа не было, значит и запрос не отработал.
func (c *OpenRouter) send(ctx context.Context, body []byte) (llmResult, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterURL, bytes.NewReader(body))
	if err != nil {
		return llmResult{}, false, fmt.Errorf("build http request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.key)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return llmResult{}, true, fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, llmBodyLimit))
	if err != nil {
		return llmResult{}, true, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return llmResult{}, retry, fmt.Errorf("status %d: %s", resp.StatusCode, errorMessage(raw))
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return llmResult{}, false, fmt.Errorf("decode response: %w", err)
	}
	// Ошибку провайдера OpenRouter кладёт в тело ответа с кодом 200.
	if out.Error != nil {
		retry := out.Error.Code == http.StatusTooManyRequests || out.Error.Code >= 500
		return llmResult{}, retry, fmt.Errorf("provider error %d: %s", out.Error.Code, cut(out.Error.Message))
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return llmResult{}, false, errors.New("empty content")
	}

	return llmResult{
		content:   json.RawMessage(out.Choices[0].Message.Content),
		tokensIn:  out.Usage.PromptTokens,
		tokensOut: out.Usage.CompletionTokens,
	}, false, nil
}

// errorMessage берёт из ответа только error.message. Рядом в metadata провайдер
// возвращает отфильтрованный вход, а транскрипт не имеет права попасть ни в
// текст ошибки, ни следом в лог.
func errorMessage(raw []byte) string {
	var out struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "non-json body"
	}
	return cut(out.Error.Message)
}

func cut(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= 200 {
		return message
	}
	return strings.ToValidUTF8(message[:200], "")
}
