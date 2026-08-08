package app

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config читается один раз при старте и дальше передаётся значением.
type Config struct {
	DatabaseURL     string
	BotToken        string
	AllowedIDs      []int64
	LogLevel        slog.Level
	Env             string
	OpenRouterKey   string
	OpenRouterProxy string
	TelegramProxy   string
	ModelMedia      string
	ModelDialog     string
	GitHubToken     string
	MediaDir        string
	MaxItems        int
	InterviewRounds int
	AudioConvert    string
}

// LoadConfig собирает конфиг из окружения. Ошибка перечисляет все проблемы
// разом: иначе поднятие контура идёт циклом «запустил, узнал про одну
// переменную, запустил снова».
func LoadConfig() (Config, error) {
	var problems []string

	cfg := Config{
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		BotToken:      os.Getenv("TELEGRAM_BOT_TOKEN"),
		Env:           valueOr(os.Getenv("ENV"), "dev"),
		OpenRouterKey: os.Getenv("OPENROUTER_API_KEY"),
		// Пусто - идём напрямую. Нужен там, где хосту закрыт прямой путь к
		// OpenRouter: без прокси запрос возвращает 403 от фильтра по дороге, а
		// не ответ API.
		OpenRouterProxy: os.Getenv("OPENROUTER_PROXY"),
		// Прокси Telegram отдельный и другой схемы: у соседних сервисов это
		// socks5, тогда как OpenRouter ходит через HTTP-прокси.
		TelegramProxy: os.Getenv("TELEGRAM_PROXY"),
		ModelMedia:    valueOr(os.Getenv("OPENROUTER_MODEL_MEDIA"), "google/gemini-3.1-flash-lite"),
		ModelDialog:   valueOr(os.Getenv("OPENROUTER_MODEL_DIALOG"), "deepseek/deepseek-v4-flash-0731"),
		GitHubToken:   os.Getenv("GITHUB_TOKEN"),
		MediaDir:      valueOr(os.Getenv("MEDIA_DIR"), "/tmp/intake"),
	}

	if cfg.DatabaseURL == "" {
		problems = append(problems, "DATABASE_URL is empty")
	}
	if cfg.BotToken == "" {
		problems = append(problems, "TELEGRAM_BOT_TOKEN is empty")
	}
	if cfg.OpenRouterKey == "" {
		problems = append(problems, "OPENROUTER_API_KEY is empty")
	}
	// Без токена сервис доводит обращение до саммари и упирается в публикацию:
	// весь разговор с автором оказался бы напрасным.
	if cfg.GitHubToken == "" {
		problems = append(problems, "GITHUB_TOKEN is empty")
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

	maxItems, err := parsePositive("MAX_ITEMS", valueOr(os.Getenv("MAX_ITEMS"), "30"))
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.MaxItems = maxItems

	rounds, err := parsePositive("INTERVIEW_ROUNDS", valueOr(os.Getenv("INTERVIEW_ROUNDS"), "3"))
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.InterviewRounds = rounds

	audioConvert, err := parseAudioConvert(valueOr(os.Getenv("AUDIO_CONVERT"), "auto"))
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.AudioConvert = audioConvert

	// Разбираем при старте: битый адрес прокси иначе всплыл бы первым вызовом
	// наружу, то есть в середине живого разговора.
	for name, value := range map[string]string{
		"OPENROUTER_PROXY": cfg.OpenRouterProxy,
		"TELEGRAM_PROXY":   cfg.TelegramProxy,
	} {
		if err := checkProxy(name, value); err != nil {
			problems = append(problems, err.Error())
		}
	}

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

// parsePositive разбирает счётные настройки: MAX_ITEMS ограничивает пачку сырья
// числом элементов, INTERVIEW_ROUNDS - число раундов вопросов. Ноль в обоих
// случаях означал бы сервис, который ничего не принимает или ничего не
// спрашивает.
func parsePositive(name, raw string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s %q is not a positive integer", name, raw)
	}
	return n, nil
}

// checkProxy проверяет адрес прокси. Пустой означает прямой путь и ошибкой не
// является: локально и на хостах без блокировок прокси не нужен.
func checkProxy(name, raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Scheme == "" {
		return fmt.Errorf("%s %q is not a proxy url like http://host:port", name, raw)
	}
	return nil
}

func parseAudioConvert(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "auto":
		return "auto", nil
	case "always":
		return "always", nil
	default:
		return "", fmt.Errorf("AUDIO_CONVERT %q is not auto or always", raw)
	}
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
