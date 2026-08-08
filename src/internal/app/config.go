package app

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// Config читается один раз при старте и дальше передаётся значением.
type Config struct {
	DatabaseURL string
	BotToken    string
	AllowedIDs  []int64
	LogLevel    slog.Level
	Env         string
}

// LoadConfig собирает конфиг из окружения. Ошибка перечисляет все проблемы
// разом: иначе поднятие контура идёт циклом «запустил, узнал про одну
// переменную, запустил снова».
func LoadConfig() (Config, error) {
	var problems []string

	cfg := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		BotToken:    os.Getenv("TELEGRAM_BOT_TOKEN"),
		Env:         valueOr(os.Getenv("ENV"), "dev"),
	}

	if cfg.DatabaseURL == "" {
		problems = append(problems, "DATABASE_URL is empty")
	}
	if cfg.BotToken == "" {
		problems = append(problems, "TELEGRAM_BOT_TOKEN is empty")
	}

	ids, err := parseIDs(os.Getenv("TELEGRAM_ALLOWED_IDS"))
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.AllowedIDs = ids

	level, err := parseLevel(valueOr(os.Getenv("LOG_LEVEL"), "info"))
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.LogLevel = level

	if len(problems) > 0 {
		return Config{}, errors.New("config: " + strings.Join(problems, "; "))
	}
	return cfg, nil
}

// parseIDs разбирает белый список. Пустой список - ошибка: бот без белого
// списка пускал бы в приватные репозитории любого, кто нашёл его в поиске.
func parseIDs(raw string) ([]int64, error) {
	var ids []int64
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("TELEGRAM_ALLOWED_IDS has non-numeric entry %q", part)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, errors.New("TELEGRAM_ALLOWED_IDS is empty")
	}
	return ids, nil
}

func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("LOG_LEVEL %q is not debug, info, warn or error", raw)
	}
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
