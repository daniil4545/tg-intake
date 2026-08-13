package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// Столько же заложено в MaxConns пула: работа держит соединение на время
	// запроса к модели.
	workerCount = 2
	// По работе за раз: лок живёт пять минут, а запрос к модели с повторами
	// идёт минутами, и вторая работа пачки протухла бы под локом ещё до
	// старта. Параллельность даёт число воркеров, а не размер пачки.
	claimLimit = 1
	// Попыток на работу; шестая переводит её в failed.
	maxAttempts = 5
	// Пустая очередь: пауза между опросами.
	pollDelay  = 2 * time.Second
	unlockTick = time.Minute
	// Бюджет работы меньше срока протухания лока: иначе уборщик отдаст другому
	// воркеру работу, которая ещё выполняется.
	jobTimeout = 4 * time.Minute
	lockStale  = 5 * time.Minute
	// Срок жизни отработавшей работы. Неделя с запасом переживает разбор
	// инцидента, а обращение к этому сроку давно опубликовано или отменено, и
	// работу с тем же ключом ему уже не поставить.
	jobTTL = 7 * 24 * time.Hour
)

// JobHandler выполняет работу одного вида. Ошибка означает повтор, nil - done.
type JobHandler func(ctx context.Context, job Job) error

// Job - строка очереди. Payload не разбирается воркером: его читает обработчик
// своего вида.
type Job struct {
	ID       int64
	Kind     string
	Key      string
	Payload  json.RawMessage
	Attempts int
}

// Runner - то общее, что есть у пула и у транзакции. Нужен, чтобы постановка
// работы шла в той же транзакции, что и смена статуса обращения.
type Runner interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// PutJob ставит работу. Ключ детерминирован, поэтому повторная постановка той
// же работы - не ошибка транзакции, а признак, что она уже стоит.
func PutJob(ctx context.Context, db Runner, kind, key string, payload any) error {
	raw := []byte("{}")
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal payload of job %s: %w", kind, err)
		}
		raw = encoded
	}

	_, err := db.Exec(ctx, `
		INSERT INTO jobs (kind, key, payload) VALUES ($1, $2, $3)
		ON CONFLICT (key) DO NOTHING`, kind, key, raw)
	if err != nil {
		return fmt.Errorf("put job %s: %w", kind, err)
	}
	return nil
}

// ClaimJobs забирает пачку готовых работ. SKIP LOCKED разводит воркеров: одна
// работа достаётся ровно одному.
func ClaimJobs(ctx context.Context, pool *pgxpool.Pool, limit int) ([]Job, error) {
	rows, err := pool.Query(ctx, `
		UPDATE jobs SET status = 'running', locked_at = now(),
		                attempts = attempts + 1, updated_at = now()
		WHERE id IN (SELECT id FROM jobs
		             WHERE status = 'pending' AND run_after <= now()
		             ORDER BY run_after LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING id, kind, key, payload, attempts`, limit)
	if err != nil {
		return nil, fmt.Errorf("claim jobs: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var job Job
		var payload []byte
		if err := rows.Scan(&job.ID, &job.Kind, &job.Key, &payload, &job.Attempts); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		job.Payload = payload
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// FinishJob гасит отработавшую работу. Признак в ответе - строки уже нет:
// работу сняли из-под воркера, пока он её выполнял (обращение двинулось дальше
// и заменило её новой). Исход при этом уже записан обработчиком, но сам факт
// обязан быть виден: незамеченным он выглядит как случайная потеря аудита.
func FinishJob(ctx context.Context, pool *pgxpool.Pool, id int64) (bool, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE jobs SET status = 'done', locked_at = NULL, last_error = NULL,
		                updated_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("finish job %d: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}

// Exhausted - повторы кончились: следующий шаг не отсрочка, а исход провала.
func (j Job) Exhausted() bool { return j.Attempts > maxAttempts }

// FailJob откладывает работу на 2^attempts секунд либо, если попытки
// исчерпаны, гасит её. Признак в ответе - работа исчерпана. Принимает пул либо
// транзакцию: гашение работы обязано лежать в одной транзакции с исходом
// провала.
func FailJob(ctx context.Context, db Runner, job Job, cause error) (bool, error) {
	if job.Exhausted() {
		_, err := db.Exec(ctx, `
			UPDATE jobs SET status = 'failed', locked_at = NULL, last_error = $2,
			                updated_at = now()
			WHERE id = $1`, job.ID, cause.Error())
		if err != nil {
			return false, fmt.Errorf("fail job %d: %w", job.ID, err)
		}
		return true, nil
	}

	_, err := db.Exec(ctx, `
		UPDATE jobs SET status = 'pending', locked_at = NULL, last_error = $2,
		                run_after = now() + ($3::int * interval '1 second'),
		                updated_at = now()
		WHERE id = $1`, job.ID, cause.Error(), 1<<job.Attempts)
	if err != nil {
		return false, fmt.Errorf("retry job %d: %w", job.ID, err)
	}
	return false, nil
}

// SweepJobs убирает отработавшие работы. Держать их вечно незачем: аудит живёт
// в case_events, а идемпотентность по ключу нужна ровно до тех пор, пока
// обращение может поставить работу заново. Провалы остаются: по ним разбирают
// инциденты.
func SweepJobs(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		DELETE FROM jobs
		WHERE status = 'done' AND updated_at < now() - $1::int * interval '1 second'`,
		int(jobTTL.Seconds()))
	if err != nil {
		return fmt.Errorf("sweep done jobs: %w", err)
	}
	return nil
}

// RunWorker держит два обработчика и уборщик локов, возвращается по ctx.
// onFail обязателен и вызывается один раз на исчерпанную работу: он гасит саму
// работу вместе с исходом провала, потому что перевод элемента в failed и
// движение цепочки дальше - дело обращения, не очереди.
func RunWorker(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, handlers map[string]JobHandler, onFail func(ctx context.Context, job Job, cause error)) {
	w := &worker{pool: pool, log: log, handlers: handlers, onFail: onFail}

	var wg sync.WaitGroup
	wg.Add(workerCount + 1)
	for range workerCount {
		go func() {
			defer wg.Done()
			w.loop(ctx)
		}()
	}
	go func() {
		defer wg.Done()
		w.unlockLoop(ctx)
	}()
	wg.Wait()
}

type worker struct {
	pool     *pgxpool.Pool
	log      *slog.Logger
	handlers map[string]JobHandler
	onFail   func(ctx context.Context, job Job, cause error)
}

func (w *worker) loop(ctx context.Context) {
	for {
		jobs, err := ClaimJobs(ctx, w.pool, claimLimit)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			w.log.Error("claim_failed", "error", err)
		}
		if len(jobs) == 0 {
			if !wait(ctx, pollDelay) {
				return
			}
			continue
		}
		for _, job := range jobs {
			w.run(ctx, job)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

func (w *worker) run(ctx context.Context, job Job) {
	w.log.Info("job_claimed", "kind", job.Kind, "attempt", job.Attempts)

	handler, ok := w.handlers[job.Kind]
	if !ok {
		// Иначе работа неизвестного вида крутится в очереди вечно и молча.
		w.fail(ctx, job, fmt.Errorf("no handler for kind %s", job.Kind))
		return
	}

	runCtx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()

	start := time.Now()
	if err := handler(runCtx, job); err != nil {
		if ctx.Err() != nil {
			// Остановка сервиса не провал работы: лок снимет unlockStale.
			return
		}
		w.fail(ctx, job, err)
		return
	}
	finished, err := FinishJob(ctx, w.pool, job.ID)
	if err != nil {
		w.log.Error("job_finish_failed", "kind", job.Kind, "error", err)
		return
	}
	if !finished {
		w.log.Warn("job_replaced_running", "kind", job.Kind, "job_id", job.ID)
	}
	w.log.Info("job_done", "kind", job.Kind, "ms", time.Since(start).Milliseconds())
}

func (w *worker) fail(ctx context.Context, job Job, cause error) {
	if !job.Exhausted() {
		if _, err := FailJob(ctx, w.pool, job, cause); err != nil {
			w.log.Error("job_fail_failed", "kind", job.Kind, "error", err)
			return
		}
		w.log.Warn("job_failed", "kind", job.Kind, "attempts", job.Attempts, "error", cause)
		return
	}

	w.log.Error("job_failed", "kind", job.Kind, "attempts", job.Attempts, "error", cause)
	// Работу гасит onFail, одной транзакцией с исходом провала: коммит порознь
	// оставил бы обращение в normalizing навсегда, а погашенную работу уборщик
	// локов уже не поднимет.
	w.onFail(ctx, job, cause)
}

func (w *worker) unlockLoop(ctx context.Context) {
	for wait(ctx, unlockTick) {
		tag, err := w.pool.Exec(ctx, `
			UPDATE jobs SET status = 'pending', locked_at = NULL, updated_at = now()
			WHERE status = 'running' AND locked_at < now() - $1::int * interval '1 second'`,
			int(lockStale.Seconds()))
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			w.log.Error("unlock_failed", "error", err)
			continue
		}
		if n := tag.RowsAffected(); n > 0 {
			// Воркер умер вместе с процессом: работы вернулись в очередь.
			w.log.Warn("jobs_unlocked", "jobs", n)
		}
	}
}

// wait возвращает false, если ждать больше незачем: сервис останавливается.
func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
