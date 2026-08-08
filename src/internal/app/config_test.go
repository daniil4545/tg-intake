package app

import (
	"slices"
	"strings"
	"testing"
)

func TestLoadConfigReportsEveryProblem(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("TELEGRAM_ALLOWED_IDS", "123,not-a-number")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("AUDIO_CONVERT", "sometimes")
	// Битый адрес прокси обязан всплыть при старте, а не первым вызовом модели
	// в середине живого разговора.
	t.Setenv("OPENROUTER_PROXY", "::not a url::")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	for _, want := range []string{"DATABASE_URL", "TELEGRAM_ALLOWED_IDS", "OPENROUTER_API_KEY", "GITHUB_TOKEN", "AUDIO_CONVERT", "OPENROUTER_PROXY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %s, got: %v", want, err)
		}
	}
}

func TestLoadConfigParsesAllowedIDs(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/intake")
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("TELEGRAM_ALLOWED_IDS", " 123 , 456,")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("ENV", "")
	t.Setenv("OPENROUTER_API_KEY", "key")
	t.Setenv("OPENROUTER_PROXY", "")
	t.Setenv("OPENROUTER_MODEL_MEDIA", "")
	t.Setenv("OPENROUTER_MODEL_DIALOG", "")
	t.Setenv("GITHUB_TOKEN", "token")
	t.Setenv("MEDIA_DIR", "")
	t.Setenv("MAX_ITEMS", "")
	t.Setenv("INTERVIEW_ROUNDS", "")
	t.Setenv("AUDIO_CONVERT", "")
	t.Setenv("PROJECTS", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("want no error, got: %v", err)
	}
	if !slices.Equal(cfg.AllowedIDs, []int64{123, 456}) {
		t.Errorf("allowed ids: got %v", cfg.AllowedIDs)
	}
	if cfg.Env != "dev" {
		t.Errorf("env fallback: got %q", cfg.Env)
	}
	if cfg.ModelMedia != "google/gemini-3.1-flash-lite" {
		t.Errorf("model media fallback: got %q", cfg.ModelMedia)
	}
	if cfg.MediaDir != "/tmp/intake" {
		t.Errorf("media dir fallback: got %q", cfg.MediaDir)
	}
	if cfg.MaxItems != 30 {
		t.Errorf("max items fallback: got %d", cfg.MaxItems)
	}
	if cfg.ModelDialog != "deepseek/deepseek-v4-flash-0731" {
		t.Errorf("model dialog fallback: got %q", cfg.ModelDialog)
	}
	if cfg.InterviewRounds != 3 {
		t.Errorf("interview rounds fallback: got %d", cfg.InterviewRounds)
	}
	if cfg.AudioConvert != "auto" {
		t.Errorf("audio convert fallback: got %q", cfg.AudioConvert)
	}
}

// TestParseProjects: список проектов приходит из конфига контура, и битая
// строка обязана всплыть в логе выката, а не пустым меню у автора.
func TestParseProjects(t *testing.T) {
	valid := `[{"slug":"crm-bot","title":"CRM","owner":"acme","repo":"crm","context":"про crm"}]`

	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"пусто - не ошибка", "", true},
		{"валидный список", valid, true},
		{"битый json", `[{"slug":`, false},
		{"slug не по маске", `[{"slug":"CRM Bot","title":"CRM","owner":"a","repo":"r","context":"c"}]`, false},
		{"дубль slug", `[{"slug":"crm","title":"CRM","owner":"a","repo":"r","context":"c"},
			{"slug":"crm","title":"CRM 2","owner":"a","repo":"r2","context":"c"}]`, false},
		{"без контекста", `[{"slug":"crm","title":"CRM","owner":"a","repo":"r","context":"  "}]`, false},
		{"без репозитория", `[{"slug":"crm","title":"CRM","owner":"a","repo":"","context":"c"}]`, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseProjects(c.raw)
			if c.ok && err != nil {
				t.Fatalf("want no error, got: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("want error, got nil")
			}
			if c.raw == valid && (len(got) != 1 || !got[0].IsActive()) {
				t.Errorf("проект по умолчанию активен: %+v", got)
			}
		})
	}
}
