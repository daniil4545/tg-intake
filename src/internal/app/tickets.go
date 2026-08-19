package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ticketsLimit - сколько тикетов на странице списка. Пять помещаются на экран
// телефона вместе с кнопками листания; остальное достаётся листанием.
const ticketsLimit = 5

// issueScan - окно, в котором ищутся метки своих тикетов. Больше сотни GitHub
// за один запрос не отдаёт.
const issueScan = 100

// Метки, которые ставит код, а не человек: начальная при публикации и метка
// отмены. Наличие обеих в правилах проверяется при старте, иначе набор
// статусов снова раздвоится между Go и файлом.
const (
	labelNew       = "status:new"
	labelCancelled = "status:cancelled"
)

var (
	// ErrNotAuthor - отменять чужой тикет нельзя. Просмотр общий, отмена нет.
	ErrNotAuthor = errors.New("ticket belongs to another author")
	// ErrIssueGone - тикета в GitHub больше нет: удалён или перенесён.
	ErrIssueGone = errors.New("issue does not exist")
)

// Ticket - тикет для показа автору. Author, Brief и Comment заполняет только
// карточка: в списке они пусты, и второй тип ради трёх пустых строк не стоит
// пересечения в шести полях.
type Ticket struct {
	CaseID string
	Number int
	URL    string
	Title  string
	UserID int64
	Status Status
	// Unavailable - GitHub не ответил, и статус не прочитан вовсе. От
	// нераспознанной метки отличается тем, что про тикет не известно ничего:
	// закрытому предлагать отмену нельзя.
	Unavailable bool
	Author      string
	// Brief - краткое содержание, основное описание в карточке. Body - тело
	// тикета: карточка берёт его начало, когда краткого нет, то есть у тикетов,
	// заведённых до его появления. Целиком тело в бота не идёт - на экране
	// телефона оно вытесняет статус и комментарий.
	Brief   string
	Body    string
	Comment string
	// News - по тикету есть новость, о которой автору сказали, а карточку он не
	// открыл. Ставится только владельцу тикета: чужая отметка ему не нужна.
	News bool
}

// Tickets - просмотр тикетов и отмена. Владелец состояния обращения по-прежнему
// Cases: здесь только то, что требует GitHub.
type Tickets struct {
	cases    *Cases
	gh       *GitHub
	statuses Statuses
	log      *slog.Logger
	// Чат уведомлений владельца; 0 - уведомления выключены.
	alertChat int64
}

func NewTickets(cases *Cases, gh *GitHub, statuses Statuses, log *slog.Logger, alertChat int64) *Tickets {
	return &Tickets{cases: cases, gh: gh, statuses: statuses, log: log, alertChat: alertChat}
}

// List - страница тикетов проекта. Номера и заголовки из своей базы, статусы
// одним запросом к GitHub. Второе значение - сколько тикетов у проекта всего:
// без него не собрать навигацию, а лишний COUNT по своей базе дешевле, чем
// «вперёд» в пустоту.
//
// Отказ GitHub не прячет список: тикеты видны без статусов. Просмотр не должен
// умирать вместе с чужим сервисом.
func (t *Tickets) List(ctx context.Context, project Project, userID int64, page int) ([]Ticket, int, error) {
	if page < 0 {
		page = 0
	}
	rows, err := t.cases.pool.Query(ctx, `
		SELECT id, issue_number, COALESCE(issue_url, ''), COALESCE(title, ''), user_id,
		       has_news AND user_id = $3, count(*) OVER ()
		FROM cases
		WHERE project_id = $1 AND issue_number IS NOT NULL
		ORDER BY issue_number DESC LIMIT $2 OFFSET $4`,
		project.ID, ticketsLimit, userID, page*ticketsLimit)
	if err != nil {
		return nil, 0, fmt.Errorf("query tickets of project %d: %w", project.ID, err)
	}
	defer rows.Close()

	var tickets []Ticket
	total := 0
	for rows.Next() {
		var t Ticket
		if err := rows.Scan(&t.CaseID, &t.Number, &t.URL, &t.Title, &t.UserID, &t.News, &total); err != nil {
			return nil, 0, fmt.Errorf("scan ticket: %w", err)
		}
		tickets = append(tickets, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read tickets: %w", err)
	}
	if len(tickets) == 0 {
		return nil, 0, nil
	}

	issues, err := t.gh.ListIssues(ctx, project, issueScan)
	if err != nil {
		t.log.Warn("issues_unavailable", "project", project.Slug, "error", err)
		return tickets, total, nil
	}

	labels := make(map[int][]string, len(issues))
	for _, issue := range issues {
		labels[issue.Number] = issue.LabelNames()
	}
	for i := range tickets {
		names, ok := labels[tickets[i].Number]
		if !ok {
			// Тикет старше окна сканирования: сотню последних номеров в живом
			// репозитории занимают и pull request'ы, вытеснить оттуда тикет
			// проще, чем кажется. Добираем поштучно - в списке их не больше
			// десяти. Первый отказ останавливает добор: GitHub деградировал, и
			// долбить его из синхронного хендлера значит морозить бота.
			issue, err := t.gh.GetIssue(ctx, project, tickets[i].Number, true)
			if err != nil {
				if isNotFound(err) {
					continue
				}
				t.log.Warn("issue_unavailable", "project", project.Slug,
					"issue", tickets[i].Number, "error", err)
				break
			}
			names = issue.LabelNames()
		}
		t.noteUnknown(project, tickets[i].Number, names)
		if status, ok := t.statuses.Pick(names); ok {
			tickets[i].Status = status
		}
	}
	return tickets, total, nil
}

// Load - карточка тикета. Тело берётся из своей базы, а не из issue.body: в
// теле тикета живут шапка авторства и служебный маркер обращения, которым
// незачем уезжать в чат.
func (t *Tickets) Load(ctx context.Context, project Project, number int) (*Ticket, error) {
	var ticket Ticket
	var summary string
	row := t.cases.pool.QueryRow(ctx, `
		SELECT id, issue_number, COALESCE(issue_url, ''), COALESCE(title, ''),
		       user_id, COALESCE(brief, ''), COALESCE(summary, ''), has_news
		FROM cases WHERE project_id = $1 AND issue_number = $2`, project.ID, number)
	if err := row.Scan(&ticket.CaseID, &ticket.Number, &ticket.URL, &ticket.Title,
		&ticket.UserID, &ticket.Brief, &summary, &ticket.News); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("load ticket %d of project %d: %w", number, project.ID, err)
	}
	// Тело нужно только как замена краткому содержанию: режет его карточка.
	if ticket.Brief == "" {
		ticket.Body = summary
	}
	author, err := LoadUser(ctx, t.cases.pool, ticket.UserID)
	if err != nil {
		return nil, err
	}
	ticket.Author = authorName(author)

	issue, err := t.gh.GetIssue(ctx, project, number, true)
	if err != nil {
		if isNotFound(err) {
			return nil, ErrIssueGone
		}
		// Карточка переживает отказ так же, как список: без статуса, но с телом.
		t.log.Warn("issue_unavailable", "project", project.Slug, "issue", number, "error", err)
		ticket.Unavailable = true
		return &ticket, nil
	}

	names := issue.LabelNames()
	t.noteUnknown(project, number, names)
	status, ok := t.statuses.Pick(names)
	if !ok {
		return &ticket, nil
	}
	ticket.Status = status

	// Комментарий читается у любого тикета, а не только у доигранного: разбор и
	// вопросы разработчика приходят задолго до финала, и у автора нет другого
	// способа их прочитать - аккаунта GitHub у него нет.
	comment, err := t.gh.LastComment(ctx, project, number)
	if err != nil {
		t.log.Warn("comments_unavailable", "project", project.Slug, "issue", number, "error", err)
		return &ticket, nil
	}
	// Молчание при финальном статусе - дыра в инварианте «отклонение всегда с
	// причиной»: у открытого тикета комментария просто может не быть.
	if comment == "" && status.Final {
		t.log.Warn("comment_missing", "project", project.Slug, "issue", number)
	}
	ticket.Comment = comment
	return &ticket, nil
}

// Page - на какой странице списка лежит тикет. Считается по номеру, а не
// запоминается в кнопке: кнопку нажимают и из сообщения недельной давности, а
// список к тому времени сдвинулся.
func (t *Tickets) Page(ctx context.Context, project Project, number int) (int, error) {
	var above int
	err := t.cases.pool.QueryRow(ctx, `
		SELECT count(*) FROM cases
		WHERE project_id = $1 AND issue_number IS NOT NULL AND issue_number > $2`,
		project.ID, number).Scan(&above)
	if err != nil {
		return 0, fmt.Errorf("count tickets above %d: %w", number, err)
	}
	return above / ticketsLimit, nil
}

// MarkSeen снимает отметку о новости: автор открыл карточку и всё прочитал.
// Автор проверяется условием запроса - чужое открытие чужую отметку не гасит.
func (t *Tickets) MarkSeen(ctx context.Context, caseID string, userID int64) error {
	_, err := t.cases.pool.Exec(ctx, `
		UPDATE cases SET has_news = false, updated_at = now()
		WHERE id = $1 AND user_id = $2 AND has_news`, caseID, userID)
	if err != nil {
		return fmt.Errorf("clear news of case %s: %w", caseID, err)
	}
	return nil
}

// Cancel ставит работу отмены. Через replaceJob: повторная отмена после
// исчерпанных повторов первой снова уходит в очередь, а не упирается в ключ
// погашенной работы. Первое значение - обращение тикета: его исход придёт
// очередью, и бот правит им тот экран, с которого отмену запустили.
func (t *Tickets) Cancel(ctx context.Context, project Project, number int, userID int64) (string, error) {
	var caseID string
	// Чтение и постановка одной транзакцией: между ними обращение может уйти в
	// отмену вторым нажатием, и работа встала бы поверх уже закрытого тикета.
	err := t.cases.inTx(ctx, func(tx pgx.Tx) error {
		var owner int64
		row := tx.QueryRow(ctx,
			`SELECT id, user_id FROM cases WHERE project_id = $1 AND issue_number = $2 FOR UPDATE`,
			project.ID, number)
		if err := row.Scan(&caseID, &owner); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrIssueGone
			}
			return fmt.Errorf("load case of issue %d: %w", number, err)
		}
		if owner != userID {
			return ErrNotAuthor
		}
		return replaceJob(ctx, tx, JobCancelIssue, caseID,
			cancelPayload{CaseID: caseID, UserID: userID})
	})
	if err != nil {
		return "", err
	}
	return caseID, nil
}

// RunCancel закрывает тикет в GitHub. Порядок вызовов важен: метка ставится до
// закрытия, иначе падение между ними оставит закрытый issue без объяснения.
func (t *Tickets) RunCancel(ctx context.Context, job Job) error {
	var p cancelPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return fmt.Errorf("payload of %s: %w", job.Kind, err)
	}

	cs, err := t.cases.Load(ctx, p.CaseID)
	if err != nil {
		return err
	}
	if cs == nil || cs.IssueNumber == 0 || cs.ProjectID == nil {
		return nil
	}
	// Право проверяется второй раз: кнопка живёт в чужом сообщении дольше, чем
	// экран, с которого её нажали.
	if cs.UserID != p.UserID {
		t.log.Warn("cancel_denied", "case_id", cs.ID, "user_id", p.UserID)
		return nil
	}

	project, err := LoadProject(ctx, t.cases.pool, *cs.ProjectID)
	if err != nil {
		return err
	}
	// Автор нужен только уведомлению владельца, и читается до первой мутации в
	// GitHub: отказ базы после закрытия issue стоил бы лишнего круга повторов.
	var author User
	if t.alertChat != 0 {
		if author, err = LoadUser(ctx, t.cases.pool, cs.UserID); err != nil {
			return err
		}
	}

	issue, err := t.gh.GetIssue(ctx, project, cs.IssueNumber, false)
	if err != nil {
		if isNotFound(err) {
			// Автору обещано «сообщу, когда закроется»: молчание про пропавший
			// issue читалось бы как зависший бот.
			return t.cases.inTx(ctx, func(tx pgx.Tx) error {
				return putNotifyKey(ctx, tx, cs.ID, "cancel-gone",
					fmt.Sprintf("Тикета #%d уже нет в GitHub, отменять нечего.", cs.IssueNumber), keysCancel)
			})
		}
		return err
	}

	// Своя метка при проверке не считается. Повтор после падения между
	// AddLabel и CloseIssue видит собственный status:cancelled, и без этого
	// исключения работа решила бы, что тикет уже закрыт, - issue остался бы
	// открытым навсегда, а автор получил бы «уже закрыт».
	names := issue.LabelNames()
	if status, ok := t.statuses.Pick(withoutLabel(names, labelCancelled)); ok && status.Final {
		return t.cases.inTx(ctx, func(tx pgx.Tx) error {
			return putNotifyKey(ctx, tx, cs.ID, "cancel-late",
				fmt.Sprintf("Тикет #%d уже закрыт со статусом «%s», отменять нечего.",
					cs.IssueNumber, status.Title), keysCancel)
		})
	}

	if err := t.gh.AddLabel(ctx, project, cs.IssueNumber, labelCancelled); err != nil {
		return err
	}
	// Снимаются все прочие метки статуса, а не одна: падение между добавлением и
	// снятием оставляет на issue две.
	for _, name := range names {
		if !strings.HasPrefix(name, statusPrefix) || name == labelCancelled {
			continue
		}
		if err := t.gh.RemoveLabel(ctx, project, cs.IssueNumber, name); err != nil {
			return err
		}
	}
	if err := t.gh.CloseIssue(ctx, project, cs.IssueNumber); err != nil {
		return err
	}

	recorded := false
	err = t.cases.inTx(ctx, func(tx pgx.Tx) error {
		// Единственность держит уникальный индекс, а не проверка перед вставкой:
		// повтор работы после падения между закрытием issue и её гашением, как и
		// вторая работа отмены рядом, обязаны дать одно событие.
		tag, err := tx.Exec(ctx, `
			INSERT INTO case_events (case_id, kind, payload)
			VALUES ($1, 'cancelled_by_author', $2::jsonb)
			ON CONFLICT DO NOTHING`,
			cs.ID, fmt.Sprintf(`{"issue": %d}`, cs.IssueNumber))
		if err != nil {
			return fmt.Errorf("record cancel of case %s: %w", cs.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		recorded = true
		if err := putNotifyKey(ctx, tx, cs.ID, "cancelled",
			fmt.Sprintf("Тикет #%d отменён и закрыт.", cs.IssueNumber), keysCancel); err != nil {
			return err
		}
		if t.alertChat == 0 {
			return nil
		}
		return putAlert(ctx, tx, cs.ID, "alert-cancel",
			alertCancelled(project, cs, author, cs.IssueNumber, issue.HTMLURL), t.alertChat)
	})
	if err != nil {
		return err
	}

	if recorded {
		t.log.Info("issue_cancelled", "case_id", cs.ID, "user_id", cs.UserID,
			"project", project.Slug, "issue", cs.IssueNumber)
	}
	return nil
}

// noteUnknown логирует метки статуса, которых нет в правилах: владелец завёл
// свою, и выдумывать её смысл сервис не станет.
func (t *Tickets) noteUnknown(project Project, number int, labels []string) {
	for _, label := range t.statuses.Unknown(labels) {
		t.log.Warn("status_unknown", "project", project.Slug, "issue", number, "label", label)
	}
}

// withoutLabel - набор меток без одной. Нужен там, где своя метка не должна
// влиять на решение.
func withoutLabel(labels []string, skip string) []string {
	kept := make([]string, 0, len(labels))
	for _, label := range labels {
		if label != skip {
			kept = append(kept, label)
		}
	}
	return kept
}

func isNotFound(err error) bool {
	var apiErr *githubError
	return errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound
}

type cancelPayload struct {
	CaseID string `json:"case_id"`
	UserID int64  `json:"user_id"`
}
