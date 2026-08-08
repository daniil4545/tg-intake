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

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	for _, want := range []string{"DATABASE_URL", "TELEGRAM_ALLOWED_IDS"} {
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
}
