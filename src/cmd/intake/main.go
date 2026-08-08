package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/daniil4545/tg-intake/internal/app"
)

// version проставляется линкером: -ldflags "-X main.version=<sha>". Приёмка
// сверяет ревизию в рантайме с выкаченным кандидатом.
var version = "dev"

func main() {
	cfg, err := app.LoadConfig()
	if err != nil {
		slog.Error("config_failed", "error", err)
		os.Exit(1)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})).
		With("service", "intake", "env", cfg.Env)

	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(cfg))
	}

	log.Info("starting", "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := app.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("database_failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	log.Info("db_connected")

	bot, err := app.NewBot(cfg, pool, log)
	if err != nil {
		log.Error("bot_failed", "error", err)
		os.Exit(1)
	}

	go func() {
		<-ctx.Done()
		log.Info("shutdown")
		bot.Stop()
	}()

	log.Info("bot_started")
	bot.Start()
}

// healthcheck - подкоманда для compose: в distroless нет ни shell, ни curl,
// проверять контейнер больше нечем.
func healthcheck(cfg app.Config) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := app.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return 1
	}
	pool.Close()
	return 0
}
