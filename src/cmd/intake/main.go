package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/daniil4545/tg-intake/internal/app"
)

// version проставляется линкером: -ldflags "-X main.version=<sha>". Приёмка
// сверяет ревизию в рантайме с выкаченным кандидатом.
var version = "dev"

// sweepPeriod - как часто убираются файлы брошенных черновиков.
const sweepPeriod = time.Hour

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

	media, err := app.NewMedia(cfg.MediaDir, log)
	if err != nil {
		log.Error("media_failed", "error", err)
		os.Exit(1)
	}

	cases := app.NewCases(pool, media, log, cfg.MaxItems)
	llm := app.NewOpenRouter(cfg.OpenRouterKey, cfg.ModelMedia, log)
	normalizer := app.NewNormalizer(cases, llm, log, cfg.AudioConvert)

	bot, err := app.NewBot(ctx, cfg, pool, cases, log)
	if err != nil {
		log.Error("bot_failed", "error", err)
		os.Exit(1)
	}

	handlers := map[string]app.JobHandler{
		app.JobNormalizeVoice:  normalizer.RunNormalizeVoice,
		app.JobNormalizeImages: normalizer.RunNormalizeImages,
		app.JobNotify:          bot.Notify,
	}

	var background sync.WaitGroup
	background.Add(2)
	go func() {
		defer background.Done()
		app.RunWorker(ctx, pool, log, handlers, cases.HandleFailedJob)
	}()
	go func() {
		defer background.Done()
		sweepDrafts(ctx, cases, log)
	}()

	go bot.Start()
	log.Info("bot_started")

	// Поллер telebot останавливается только явным Stop, воркер - по ctx.
	// Ждём воркера до pool.Close: работа в полёте не должна упереться в
	// закрытый пул и уехать в лишний повтор.
	<-ctx.Done()
	log.Info("shutdown")
	bot.Stop()
	background.Wait()
}

// sweepDrafts раз в час стирает файлы черновиков старше суток: медиа не
// переживает обращение, а автор, забывший про пачку, вернётся к тексту.
func sweepDrafts(ctx context.Context, cases *app.Cases, log *slog.Logger) {
	ticker := time.NewTicker(sweepPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := cases.SweepDrafts(ctx); err != nil {
				log.Error("sweep_failed", "error", err)
			}
		}
	}
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
