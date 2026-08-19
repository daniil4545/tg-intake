package app

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Watch - слежение за тикетами: замечает смену метки и новый комментарий и зовёт
// автора коротким сообщением. Читает GitHub, пишет только своё состояние
// доставки и работу notify - то же, что делает напоминание о черновиках.
//
// Статус тикета сервис по-прежнему не хранит: told_status отвечает на вопрос «о
// чём автору уже сказали», а не «какой у тикета статус». Показ читает метки из
// GitHub в момент показа.
type Watch struct {
	pool     *pgxpool.Pool
	gh       *GitHub
	statuses Statuses
	log      *slog.Logger
}

func NewWatch(pool *pgxpool.Pool, gh *GitHub, statuses Statuses, log *slog.Logger) *Watch {
	return &Watch{pool: pool, gh: gh, statuses: statuses, log: log}
}

// watched - тикет под слежением вместе с тем, что о нём уже доставлено.
type watched struct {
	caseID    string
	number    int
	told      *string
	commentID int64
}

// watchProject - потолок одного проекта в обходе. Два запроса к GitHub с
// повторами укладываются в него, а зависший проект не забирает время остальных.
const watchProject = 45 * time.Second

// Run - один обход всех активных проектов. Отказ по проекту логируется и не
// останавливает остальные: недоступный репозиторий не должен глушить чужие
// новости.
func (w *Watch) Run(ctx context.Context) error {
	projects, err := ListProjects(ctx, w.pool)
	if err != nil {
		return fmt.Errorf("list projects for watch: %w", err)
	}
	for _, p := range projects {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Свой потолок на проект: без него медленный GitHub на первом проекте
		// съедал бы бюджет тика целиком, и последние по алфавиту проекты
		// голодали бы каждый раз, а не один тик.
		one, cancel := context.WithTimeout(ctx, watchProject)
		err := w.project(one, p)
		cancel()
		if err != nil {
			w.log.Warn("watch_project_failed", "project", p.Slug, "error", err)
		}
	}
	return nil
}

func (w *Watch) project(ctx context.Context, p Project) error {
	tickets, err := w.tickets(ctx, p)
	if err != nil {
		return err
	}
	if len(tickets) == 0 {
		return nil
	}

	issues, err := w.gh.ListUpdated(ctx, p, issueScan)
	if err != nil {
		return fmt.Errorf("list updated issues: %w", err)
	}
	labels := make(map[int][]string, len(issues))
	for _, issue := range issues {
		labels[issue.Number] = issue.LabelNames()
	}

	news := 0
	for _, t := range tickets {
		told, err := w.status(ctx, p, t, labels[t.number])
		if err != nil {
			return err
		}
		if told {
			news++
		}
	}

	comments, err := w.gh.ListComments(ctx, p, p.CommentsSince)
	if err != nil {
		return fmt.Errorf("list repo comments: %w", err)
	}
	byNumber := make(map[int]watched, len(tickets))
	for _, t := range tickets {
		byNumber[t.number] = t
	}

	var border time.Time
	for _, comment := range comments {
		// Граница двигается по всем рассмотренным, а не только по доставленным:
		// иначе комментарии по чужим тикетам оставались бы в окне навсегда и
		// каждый тик перечитывал их заново. Время правки, а не создания: since у
		// GitHub считает по нему же.
		if comment.UpdatedAt.After(border) {
			border = comment.UpdatedAt
		}
		// Комментарий по тикету, которого нет в базе: issue заведён в
		// репозитории руками, адресата у новости нет.
		t, ok := byNumber[comment.IssueNumber()]
		if !ok {
			continue
		}
		if comment.ID <= t.commentID {
			continue
		}
		if err := w.comment(ctx, p, t, comment); err != nil {
			return err
		}
		news++
	}
	// Граница двигается только после разбора: отказ на любом шаге выше оставляет
	// её на месте, и пропущенное доедет следующим тиком. Дубль при перекрытии
	// окна ловят told_comment_id и ключ работы.
	if !border.IsZero() {
		if err := MoveCommentsSince(ctx, w.pool, p.ID, border); err != nil {
			return err
		}
	}

	w.log.Info("watch_tick", "project", p.Slug, "tickets", len(tickets),
		"issues", len(issues), "comments", len(comments), "news", news)
	return nil
}

// tickets - тикеты проекта вместе с состоянием доставки.
func (w *Watch) tickets(ctx context.Context, p Project) ([]watched, error) {
	rows, err := w.pool.Query(ctx, `
		SELECT id, issue_number, told_status, told_comment_id
		FROM cases
		WHERE project_id = $1 AND issue_number IS NOT NULL`, p.ID)
	if err != nil {
		return nil, fmt.Errorf("query watched tickets of project %d: %w", p.ID, err)
	}
	defer rows.Close()

	var tickets []watched
	for rows.Next() {
		var t watched
		if err := rows.Scan(&t.caseID, &t.number, &t.told, &t.commentID); err != nil {
			return nil, fmt.Errorf("scan watched ticket: %w", err)
		}
		tickets = append(tickets, t)
	}
	return tickets, rows.Err()
}

// status разбирает метки одного тикета. Возвращает true, если автору поставлена
// новость.
func (w *Watch) status(ctx context.Context, p Project, t watched, names []string) (bool, error) {
	if len(names) == 0 {
		// Тикет не попал в окно последних изменённых: за прошедший тик его никто
		// не трогал, и разбирать нечего.
		return false, nil
	}
	status, ok := w.statuses.Pick(names)
	if !ok {
		return false, nil
	}
	if t.told != nil && *t.told == status.Label {
		return false, nil
	}

	// Первое наблюдение молчит: иначе выкат разослал бы новость по каждому
	// тикету, заведённому до появления слежения.
	if t.told == nil {
		if _, err := w.pool.Exec(ctx,
			`UPDATE cases SET told_status = $2, updated_at = now() WHERE id = $1`,
			t.caseID, status.Label); err != nil {
			return false, fmt.Errorf("save first status of case %s: %w", t.caseID, err)
		}
		w.log.Info("ticket_status_seen", "case_id", t.caseID, "project", p.Slug,
			"issue", t.number, "status", status.Label)
		return false, nil
	}

	told := status.Notify
	err := inTx(ctx, w.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE cases SET told_status = $2, has_news = has_news OR $3, updated_at = now()
			WHERE id = $1`, t.caseID, status.Label, told); err != nil {
			return fmt.Errorf("save status of case %s: %w", t.caseID, err)
		}
		if !told {
			return nil
		}
		return putNotifyKey(ctx, tx, t.caseID, "status:"+status.Label,
			statusNews(p, t.number, status), keysTicket)
	})
	if err != nil {
		return false, err
	}
	w.log.Info("ticket_status_told", "case_id", t.caseID, "project", p.Slug,
		"issue", t.number, "status", status.Label, "told", told)
	return told, nil
}

// comment ставит новость о комментарии. Текст комментария в сообщение не идёт:
// его читают в карточке, где он виден вместе со статусом и сутью тикета.
func (w *Watch) comment(ctx context.Context, p Project, t watched, c Comment) error {
	err := inTx(ctx, w.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE cases SET told_comment_id = $2, has_news = true, updated_at = now()
			WHERE id = $1 AND told_comment_id < $2`, t.caseID, c.ID); err != nil {
			return fmt.Errorf("save comment mark of case %s: %w", t.caseID, err)
		}
		return putNotifyKey(ctx, tx, t.caseID, "comment:"+strconv.FormatInt(c.ID, 10),
			commentNews(p, t.number), keysTicket)
	})
	if err != nil {
		return err
	}
	w.log.Info("ticket_comment_told", "case_id", t.caseID, "project", p.Slug,
		"issue", t.number, "comment", c.ID)
	return nil
}

// Тексты новостей короткие намеренно: сообщение несёт факт и кнопку перехода, а
// содержание автор читает в карточке. Иначе десяток активных тикетов превращает
// чат в ленту, которую перестают читать.
func statusNews(p Project, number int, s Status) string {
	return fmt.Sprintf("Тикет #%d (%s): статус «%s».\n%s", number, p.Title, s.Title, newsTail)
}

func commentNews(p Project, number int) string {
	return fmt.Sprintf("По тикету #%d (%s) появился комментарий разработчика.\n%s",
		number, p.Title, newsTail)
}

const newsTail = "Открыть - кнопкой ниже или в «Мои тикеты»."
