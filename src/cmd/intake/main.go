package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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

	// Правила проверяются до первого обращения: битый контракт обнаружился бы на
	// живом диалоге, а не в логе выката.
	rules, err := app.LoadContract()
	if err != nil {
		log.Error("contract_failed", "error", err)
		os.Exit(1)
	}

	cases := app.NewCases(pool, media, log, cfg.MaxItems)
	llm := app.NewOpenRouter(cfg.OpenRouterKey, cfg.ModelMedia, cfg.OpenRouterProxy, log)
	log.Info("openrouter_ready", "proxy", cfg.OpenRouterProxy != "")
	normalizer := app.NewNormalizer(cases, llm, log, cfg.AudioConvert)
	interview := app.NewInterview(cases, llm, log, rules, cfg.ModelDialog, cfg.InterviewRounds)
	github := app.NewGitHub(cfg.GitHubToken, log)
	publisher := app.NewPublisher(cases, github, rules, log)

	checkGitHub(ctx, pool, github, log)

	bot, err := app.NewBot(ctx, cfg, pool, cases, log)
	if err != nil {
		log.Error("bot_failed", "error", err)
		os.Exit(1)
	}

	handlers := map[string]app.JobHandler{
		app.JobNormalizeVoice:  normalizer.RunNormalizeVoice,
		app.JobNormalizeImages: normalizer.RunNormalizeImages,
		app.JobInterview:       interview.Run,
		app.JobSummarize:       interview.Summarize,
		app.JobPublish:         publisher.Run,
		app.JobNotify:          bot.Notify,
	}

	// Первым делом после старта: обращение, потерявшее свою работу, не
	// двигается ничем и ничего не сообщает - ни автору, ни в лог.
	if err := cases.RecoverStuck(ctx); err != nil {
		log.Error("recover_failed", "error", err)
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

// sweepDrafts раз в час стирает файлы черновиков старше суток и напоминает про
// брошенные обращения: медиа не переживает обращение, а автор, забывший про
// разговор, получает ровно одно напоминание.
func sweepDrafts(ctx context.Context, cases *app.Cases, log *slog.Logger) {
	ticker := time.NewTicker(sweepPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := cases.RecoverStuck(ctx); err != nil {
				log.Error("recover_failed", "error", err)
			}
			// Напоминание идёт до уборки: автору честнее узнать про удалённые
			// вложения тем же сообщением, которым его зовут вернуться.
			if err := cases.RemindDrafts(ctx); err != nil {
				log.Error("remind_failed", "error", err)
			}
			if err := cases.SweepDrafts(ctx); err != nil {
				log.Error("sweep_failed", "error", err)
			}
		}
	}
}

// checkGitHub заводит метки каждого активного проекта, и этим же проверяет
// право писать: метка создаётся записью. Чтение репозитория ничего не доказало
// бы - публичный репозиторий читает кто угодно.
//
// Отказ по одному проекту не мешает остальным работать, поэтому это
// предупреждение, а не падение старта. Успех снимает bootstrap с горячего пути:
// первый тикет проекта не тратит запросы на существующие метки.
func checkGitHub(ctx context.Context, pool *pgxpool.Pool, github *app.GitHub, log *slog.Logger) {
	projects, err := app.ListProjects(ctx, pool)
	if err != nil {
		log.Error("projects_failed", "error", err)
		return
	}
	for _, p := range projects {
		if err := github.PrepareProject(ctx, p); err != nil {
			log.Warn("github_write_denied", "project", p.Slug, "repo", p.Owner+"/"+p.Repo, "error", err)
			continue
		}
		if !p.LabelsReady {
			if err := app.MarkLabelsReady(ctx, pool, p.ID); err != nil {
				log.Error("labels_ready_failed", "project", p.Slug, "error", err)
			}
		}
		log.Info("github_write_ok", "project", p.Slug)
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
