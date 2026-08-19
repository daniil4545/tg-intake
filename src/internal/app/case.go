package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	tele "gopkg.in/telebot.v4"
)

// Виды работ очереди. Нормализация идёт по элементу - запись и скриншот
// получают свою работу, - а закрывает её отдельная finish_normalize: бюджет
// работы принадлежит одному вызову модели, и повтор трогает только сорвавшийся
// элемент.
const (
	JobNormalizeVoice  = "normalize_voice"
	JobNormalizeImage  = "normalize_image"
	JobFinishNormalize = "finish_normalize"
	JobInterview       = "interview"
	JobSummarize       = "summarize"
	JobPublish         = "publish"
	JobNotify          = "notify"
	JobCancelIssue     = "cancel_issue"
	JobLookup          = "lookup"
)

// caseJobKinds - работы, которые принадлежат разговору и гасятся вместе с ним.
// publish не входит: отмена из publishing запрещена, к этому моменту автор уже
// подтвердил публикацию.
var caseJobKinds = []string{JobNormalizeVoice, JobNormalizeImage, JobFinishNormalize,
	JobInterview, JobSummarize, JobLookup}

// Режим разговора. Переход односторонний: вопрос становится тикетом, обратно -
// нет.
const (
	modeTicket = "ticket"
	modeAsk    = "ask"
)

const (
	statusCollecting  = "collecting"
	statusNormalizing = "normalizing"
	statusInterview   = "interview"
	statusSummary     = "summary"
	statusPublishing  = "publishing"
	statusCancelled   = "cancelled"
	statusPublished   = "published"
	statusAnswering   = "answering"
	statusAnswered    = "answered"
)

// draftTTL - после этого срока черновик считается брошенным и теряет файлы.
// Статус при этом не меняется: текст автору сохраняем, медиа не переживает
// обращение.
const draftTTL = 24 * time.Hour

// Исходы, о которых автору отвечает bot.go. Тексты Telegram живут там же, здесь
// только причина: case.go не знает ни про кнопки, ни про формулировки.
var (
	ErrNotCollecting   = errors.New("case is not collecting")
	ErrUnknownProject  = errors.New("project is unknown or inactive")
	ErrNoProject       = errors.New("case has no project")
	ErrNoItems         = errors.New("case has no items")
	ErrNoNewItems      = errors.New("case has no new items")
	ErrTooManyItems    = errors.New("case reached item limit")
	ErrUnsupportedItem = errors.New("message kind is not supported")
	ErrPublishing      = errors.New("case is already publishing")

	// errLimitReported - лимит исчерпан, и автору об этом уже сказали: дальше
	// сообщения гасятся молча, иначе отказ приходит на каждое следующее.
	errLimitReported = errors.New("item limit is already reported")
)

// Case - обращение. ProjectID необязателен: обращение рождается раньше, чем
// автор выбрал проект, и пустой проект допустим только в collecting.
//
// Filled и Gaps - состояние контракта готовности: что интервью уже закрыло и
// что осталось пробелом. Round - номер последнего заданного раунда вопросов.
type Case struct {
	ID        string
	UserID    int64
	ProjectID *int64
	Status    string
	Mode      string
	Protocol  string
	Kind      string
	Filled    map[string]string
	Gaps      []string
	Round     int
	Title     string
	Summary   string
	// Brief - краткое содержание: суть обращения в одном-двух предложениях. Идёт
	// первым разделом в тело issue и заменяет полное описание в карточке бота.
	Brief       string
	Incomplete  bool
	IssueNumber int
}

// Item - элемент сырья. Forwarded помечает пересылку: модель должна знать, что
// это не слова автора, и уточнять деталь, а не считать её сказанной лично.
type Item struct {
	ID         int64
	Kind       string
	SourceText string
	Normalized string
	FilePath   string
	Mime       string
	Status     string
	Error      string
	Forwarded  bool
}

// Cases - жизненный цикл обращения: состояние в БД, файлы и постановка работ.
// alertChat нужен одному исходу - потерянному уведомлению: сообщение, которое
// не дошло до автора, обязан увидеть хоть кто-то.
type Cases struct {
	pool      *pgxpool.Pool
	media     *Media
	log       *slog.Logger
	maxItems  int
	alertChat int64
}

func NewCases(pool *pgxpool.Pool, media *Media, log *slog.Logger, maxItems int, alertChat int64) *Cases {
	return &Cases{pool: pool, media: media, log: log, maxItems: maxItems, alertChat: alertChat}
}

// txRunner - пул и транзакция разом. Постановка работы обязана идти в той же
// транзакции, что и смена статуса, а неразобранное сырьё - читаться там же.
type txRunner interface {
	Runner
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const caseColumns = `id, user_id, project_id, status, mode, protocol, COALESCE(kind, ''),
	contract, gaps, round, COALESCE(title, ''), COALESCE(summary, ''), COALESCE(brief, ''),
	incomplete, COALESCE(issue_number, 0)`

// Load читает обращение по идентификатору: шаги нормализации получают из
// payload только id.
func (c *Cases) Load(ctx context.Context, caseID string) (*Case, error) {
	row := c.pool.QueryRow(ctx, `SELECT `+caseColumns+` FROM cases WHERE id = $1`, caseID)
	return scanCase(row)
}

// Active возвращает активное обращение автора либо nil. Активное одно: этого
// требует частичный уникальный индекс, и на нём же держится маршрутизация
// каждого входящего сообщения.
func (c *Cases) Active(ctx context.Context, userID int64) (*Case, error) {
	row := c.pool.QueryRow(ctx, `
		SELECT `+caseColumns+` FROM cases
		WHERE user_id = $1 AND status NOT IN ('published', 'cancelled', 'answered')`, userID)
	return scanCase(row)
}

func scanCase(row pgx.Row) (*Case, error) {
	var cs Case
	var filled, gaps []byte
	err := row.Scan(&cs.ID, &cs.UserID, &cs.ProjectID, &cs.Status, &cs.Mode, &cs.Protocol, &cs.Kind,
		&filled, &gaps, &cs.Round, &cs.Title, &cs.Summary, &cs.Brief, &cs.Incomplete,
		&cs.IssueNumber)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan case: %w", err)
	}
	if err := json.Unmarshal(filled, &cs.Filled); err != nil {
		return nil, fmt.Errorf("decode contract of case %s: %w", cs.ID, err)
	}
	if err := json.Unmarshal(gaps, &cs.Gaps); err != nil {
		return nil, fmt.Errorf("decode gaps of case %s: %w", cs.ID, err)
	}
	if cs.Filled == nil {
		cs.Filled = map[string]string{}
	}
	return &cs, nil
}

// StartCase заводит обращение либо возвращает уже живое. Второе значение - оно
// уже было: автору предлагают продолжить или начать заново, и решать это боту.
//
// Неизвестный slug не отменяет старт: обращение заводится без проекта, и вопрос
// о проекте задаётся заново. Терять первое сообщение из-за устаревшей кнопки
// нельзя.
func (c *Cases) StartCase(ctx context.Context, u User, projectSlug, mode string) (*Case, bool, error) {
	if err := UpsertUser(ctx, c.pool, u); err != nil {
		return nil, false, err
	}

	existing, err := c.Active(ctx, u.ID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return existing, true, nil
	}

	var projectID *int64
	if projectSlug != "" {
		id, err := c.projectID(ctx, projectSlug)
		switch {
		case errors.Is(err, ErrUnknownProject):
			c.log.Warn("unknown_project", "user_id", u.ID, "slug", projectSlug)
		case err != nil:
			return nil, false, err
		default:
			projectID = &id
		}
	}

	cs := &Case{UserID: u.ID, ProjectID: projectID, Status: statusCollecting, Mode: mode}
	err = c.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO cases (user_id, project_id, status, mode)
			VALUES ($1, $2, 'collecting', $3) RETURNING id`, u.ID, projectID, mode).Scan(&cs.ID)
		if err != nil {
			return fmt.Errorf("insert case: %w", err)
		}
		return addEvent(ctx, tx, cs.ID, "case_started",
			map[string]any{"project": projectSlug, "mode": mode})
	})
	if err != nil {
		return nil, false, err
	}

	c.log.Info("case_started", "user_id", u.ID, "case_id", cs.ID, "project", projectSlug, "mode", mode)
	return cs, false, nil
}

// SetProject доводит выбор проекта до обращения. Выбор может опоздать
// относительно нескольких сообщений пачки, поэтому единственный источник истины
// по проекту - строка в cases, а не последний показанный экран.
func (c *Cases) SetProject(ctx context.Context, cs *Case, projectSlug string) error {
	if cs.Status != statusCollecting {
		return ErrNotCollecting
	}

	id, err := c.projectID(ctx, projectSlug)
	if err != nil {
		if errors.Is(err, ErrUnknownProject) {
			c.log.Warn("unknown_project", "user_id", cs.UserID, "slug", projectSlug)
		}
		return err
	}

	err = c.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE cases SET project_id = $2, updated_at = now()
			WHERE id = $1 AND status = 'collecting'`, cs.ID, id)
		if err != nil {
			return fmt.Errorf("set project of case %s: %w", cs.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotCollecting
		}
		return addEvent(ctx, tx, cs.ID, "project_set", map[string]any{"project": projectSlug})
	})
	if err != nil {
		return err
	}

	cs.ProjectID = &id
	c.log.Info("project_set", "user_id", cs.UserID, "case_id", cs.ID, "project", projectSlug)
	return nil
}

func (c *Cases) projectID(ctx context.Context, slug string) (int64, error) {
	var id int64
	err := c.pool.QueryRow(ctx, `SELECT id FROM projects WHERE slug = $1 AND active`, slug).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrUnknownProject
	}
	if err != nil {
		return 0, fmt.Errorf("find project %s: %w", slug, err)
	}
	return id, nil
}

// CollectItem принимает одно сообщение в сырьё. Первое значение - надо спросить
// проект: вопрос задаётся ровно один раз, признак - счётчик элементов, а не
// флаг в памяти, иначе пачка из десяти сообщений даст десять вопросов, а
// рестарт между ними - одиннадцатый.
//
// Ответа автору на принятый элемент нет: бот не перебивает.
func (c *Cases) CollectItem(ctx context.Context, bot *tele.Bot, cs *Case, msg *tele.Message) (bool, error) {
	if cs.Status != statusCollecting {
		return false, ErrNotCollecting
	}

	kind, file, mime, ok := classify(msg)
	if !ok {
		c.log.Info("item_rejected", "user_id", cs.UserID, "case_id", cs.ID, "reason", "unsupported")
		return false, ErrUnsupportedItem
	}

	count, err := c.CountItems(ctx, cs.ID)
	if err != nil {
		return false, err
	}
	if count >= c.maxItems {
		c.log.Info("item_rejected", "user_id", cs.UserID, "case_id", cs.ID, "reason", "limit")
		return false, c.noteLimit(ctx, cs.ID)
	}
	// Размер известен из апдейта, поэтому отказ не стоит ни запроса к Bot API,
	// ни строки в case_items: непринятый элемент не должен потом всплыть в
	// протоколе как «не удалось разобрать».
	if file != nil && file.FileSize > maxFileSize {
		c.log.Info("item_rejected", "user_id", cs.UserID, "case_id", cs.ID, "reason", "too_big")
		return false, ErrFileTooBig
	}

	text := msg.Text
	var tgFileID any
	if file != nil {
		text = msg.Caption
		tgFileID = file.FileID
	}

	// Вопрос о проекте решаем до вставки: строка элемента появится и при
	// провалившемся скачивании, а признак вопроса - счётчик, и второго шанса
	// спросить уже не будет.
	askProject := cs.ProjectID == nil && count == 0

	var itemID int64
	err = c.pool.QueryRow(ctx, `
		INSERT INTO case_items (case_id, kind, tg_message_id, tg_file_id, tg_group_id,
		                        source_text, mime, forwarded)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		cs.ID, kind, msg.ID, tgFileID, nullable(msg.AlbumID), text, nullable(mime),
		msg.Origin != nil).Scan(&itemID)
	if err != nil {
		return false, fmt.Errorf("insert item of case %s: %w", cs.ID, err)
	}

	if file != nil {
		if err := c.download(ctx, bot, cs, itemID, *file); err != nil {
			return askProject, err
		}
	}

	c.log.Info("item_collected", "user_id", cs.UserID, "case_id", cs.ID,
		"kind", kind, "forwarded", msg.Origin != nil, "chars", utf8.RuneCountInString(text))

	// Первый элемент обращения без проекта - единственный момент, когда бот
	// перебивает автора вопросом.
	return askProject, nil
}

// noteLimit решает, говорить ли автору про исчерпанный лимит: событие в
// case_events - признак, что уже сказали. Счётчик элементов для этого не
// годится, он на отказе не растёт.
func (c *Cases) noteLimit(ctx context.Context, caseID string) error {
	tag, err := c.pool.Exec(ctx, `
		INSERT INTO case_events (case_id, kind)
		SELECT $1, 'limit_reached'
		WHERE NOT EXISTS (
			SELECT 1 FROM case_events WHERE case_id = $1 AND kind = 'limit_reached')`, caseID)
	if err != nil {
		return fmt.Errorf("note limit of case %s: %w", caseID, err)
	}
	if tag.RowsAffected() == 0 {
		return errLimitReported
	}
	return ErrTooManyItems
}

// download кладёт вложение рядом с уже созданным элементом: имя файла - id
// элемента, а он известен только после вставки.
func (c *Cases) download(ctx context.Context, bot *tele.Bot, cs *Case, itemID int64, file tele.File) error {
	path, err := c.media.Download(bot, cs.ID, strconv.FormatInt(itemID, 10), file)
	if err != nil {
		c.log.Warn("item_rejected", "user_id", cs.UserID, "case_id", cs.ID, "reason", "download_failed", "error", err)
		if failErr := failItem(ctx, c.pool, itemID, "файл не скачался"); failErr != nil {
			c.log.Error("item_fail_failed", "case_id", cs.ID, "item_id", itemID, "error", failErr)
		}
		return fmt.Errorf("download item %d: %w", itemID, err)
	}

	_, err = c.pool.Exec(ctx, `
		UPDATE case_items SET file_path = $2, updated_at = now() WHERE id = $1`, itemID, path)
	if err != nil {
		return fmt.Errorf("set file path of item %d: %w", itemID, err)
	}
	return nil
}

// classify определяет вид элемента по содержимому сообщения. Скриншот,
// отправленный документом, - обычный случай, и терять его из-за способа
// отправки нельзя.
func classify(msg *tele.Message) (kind string, file *tele.File, mime string, ok bool) {
	switch {
	case msg.Voice != nil:
		return "voice", &msg.Voice.File, valueOr(msg.Voice.MIME, "audio/ogg"), true
	case msg.Photo != nil:
		// У Photo нет mime_type: Telegram отдаёт фото только как JPEG.
		return "photo", &msg.Photo.File, "image/jpeg", true
	case msg.Document != nil && strings.HasPrefix(msg.Document.MIME, "image/"):
		return "photo", &msg.Document.File, msg.Document.MIME, true
	case msg.Text != "":
		if hasURL(msg.Entities) {
			return "link", nil, "", true
		}
		return "text", nil, "", true
	}
	return "", nil, "", false
}

func hasURL(entities tele.Entities) bool {
	for _, e := range entities {
		if e.Type == tele.EntityURL || e.Type == tele.EntityTextLink {
			return true
		}
	}
	return false
}

// CountItems - сколько сырья в обращении. Отклик сбора берёт число отсюда:
// своё число в памяти врало бы после рестарта и после возврата обращения в сбор.
func (c *Cases) CountItems(ctx context.Context, caseID string) (int, error) {
	var count int
	err := c.pool.QueryRow(ctx, `SELECT count(*) FROM case_items WHERE case_id = $1`, caseID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count items of case %s: %w", caseID, err)
	}
	return count, nil
}

// Items отдаёт элементы обращения в порядке создания: этот порядок и есть
// порядок протокола.
func (c *Cases) Items(ctx context.Context, caseID string) ([]Item, error) {
	return loadItems(ctx, c.pool, caseID)
}

// loadItems - те же элементы из чужой транзакции: переход в тикет решает по
// сырью, чем продолжить разговор, и читать его обязан там же, где меняет статус.
func loadItems(ctx context.Context, db txRunner, caseID string) ([]Item, error) {
	rows, err := db.Query(ctx, `
		SELECT id, kind, source_text, normalized, COALESCE(file_path, ''),
		       COALESCE(mime, ''), status, COALESCE(error, ''), forwarded
		FROM case_items WHERE case_id = $1 ORDER BY id`, caseID)
	if err != nil {
		return nil, fmt.Errorf("query items of case %s: %w", caseID, err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Kind, &it.SourceText, &it.Normalized, &it.FilePath,
			&it.Mime, &it.Status, &it.Error, &it.Forwarded); err != nil {
			return nil, fmt.Errorf("scan item: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// failItem гасит элемент с причиной. Причина видна автору в протоколе строкой
// «не удалось разобрать»: пробел лучше правдоподобной выдумки. Принимает пул
// либо транзакцию: исход провала работы гасит элемент вместе с самой работой.
func failItem(ctx context.Context, db Runner, itemID int64, reason string) error {
	_, err := db.Exec(ctx, `
		UPDATE case_items SET status = 'failed', error = $2, updated_at = now()
		WHERE id = $1 AND status = 'pending'`, itemID, reason)
	if err != nil {
		return fmt.Errorf("fail item %d: %w", itemID, err)
	}
	return nil
}

// FinishCollect закрывает сбор и запускает цепочку нормализации.
func (c *Cases) FinishCollect(ctx context.Context, cs *Case) error {
	if cs.Status != statusCollecting {
		return ErrNotCollecting
	}

	count, err := c.CountItems(ctx, cs.ID)
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNoItems
	}
	if cs.ProjectID == nil {
		return ErrNoProject
	}
	// Разговор режима вопроса возвращается в сбор с прежним сырьём, поэтому
	// счётчика элементов мало: без сверки с разобранным повторное «Готово» гнало
	// бы модель по тому же вопросу второй раз.
	if cs.Mode == modeAsk {
		fresh, err := hasNewRaw(ctx, c.pool, cs.ID, cs.Protocol)
		if err != nil {
			return err
		}
		if !fresh {
			return ErrNoNewItems
		}
	}

	var voice int
	err = c.inTx(ctx, func(tx pgx.Tx) error {
		// Условие на статус в самом UPDATE: повторное «Готово» не должно
		// поставить те же работы второй раз.
		tag, err := tx.Exec(ctx, `
			UPDATE cases SET status = 'normalizing', updated_at = now()
			WHERE id = $1 AND status = 'collecting'`, cs.ID)
		if err != nil {
			return fmt.Errorf("finish collect of case %s: %w", cs.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotCollecting
		}

		// Работы нормализации привязаны к элементу: их ключ несёт item_id, и
		// работа прошлого захода молча съела бы новую через ON CONFLICT DO
		// NOTHING. Снимаем их здесь, чтобы возвращённое в сбор обращение снова
		// дошло до конца. Аудит остаётся в case_events.
		_, err = tx.Exec(ctx, `
			DELETE FROM jobs WHERE kind = ANY($2) AND payload->>'case_id' = $1`,
			cs.ID, []string{JobNormalizeVoice, JobNormalizeImage})
		if err != nil {
			return fmt.Errorf("clear normalize jobs of case %s: %w", cs.ID, err)
		}

		ids, err := pendingItems(ctx, tx, cs.ID, "voice")
		if err != nil {
			return err
		}
		voice = len(ids)
		if err := advanceNormalize(ctx, tx, cs.ID); err != nil {
			return err
		}
		return addEvent(ctx, tx, cs.ID, "collect_finished", map[string]any{"items": count})
	})
	if err != nil {
		return err
	}

	cs.Status = statusNormalizing
	c.log.Info("collect_finished", "user_id", cs.UserID, "case_id", cs.ID, "items", count, "voice", voice)
	return nil
}

// hasNewRaw - есть ли в обращении содержательное сырьё, не дошедшее до
// протокола: неразобранное вложение либо материал, добавленный после прошлого
// разбора. Погибший элемент не в счёт - «не удалось разобрать» это запись о
// потере, а не вопрос, и гнать по ней ходы модели незачем.
func hasNewRaw(ctx context.Context, db txRunner, caseID, protocol string) (bool, error) {
	items, err := loadItems(ctx, db, caseID)
	if err != nil {
		return false, err
	}
	for _, it := range items {
		if it.Status == "failed" {
			continue
		}
		if it.Status == "pending" && (it.Kind == "voice" || it.Kind == "photo") {
			return true, nil
		}
		// Строка элемента ищется в разобранном протоколе целиком: он собирается
		// теми же itemLine, и чего в нём нет - то и есть новое.
		if line := itemLine(it); line != "" && !strings.Contains(protocol, line) {
			return true, nil
		}
	}
	return false, nil
}

func pendingItems(ctx context.Context, db txRunner, caseID, kind string) ([]int64, error) {
	rows, err := db.Query(ctx, `
		SELECT id FROM case_items
		WHERE case_id = $1 AND kind = $2 AND status = 'pending' ORDER BY id`, caseID, kind)
	if err != nil {
		return nil, fmt.Errorf("query pending %s of case %s: %w", kind, caseID, err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan pending %s: %w", kind, err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AdvanceNormalize двигает цепочку нормализации - и после успеха элемента, и
// после его окончательного провала.
func (c *Cases) AdvanceNormalize(ctx context.Context, caseID string) error {
	return advanceNormalize(ctx, c.pool, caseID)
}

// advanceNormalize - единственная точка выбора следующего шага нормализации.
// Смотрит на статус элементов, а не на счётчик работ: битая запись не имеет
// права держать обращение в normalizing навсегда.
//
// Записи идут раньше скриншотов: разбор экрана требует контекста жалобы, и
// протокол на этот момент обязан быть собран. Разобрано всё - нормализацию
// закрывает finish_normalize, единственный шаг, который знает, что сырья
// больше нет.
func advanceNormalize(ctx context.Context, db txRunner, caseID string) error {
	for _, kind := range []struct{ item, job string }{
		{"voice", JobNormalizeVoice},
		{"photo", JobNormalizeImage},
	} {
		ids, err := pendingItems(ctx, db, caseID, kind.item)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			continue
		}
		for _, id := range ids {
			key := fmt.Sprintf("%s:%s:%d", kind.job, caseID, id)
			if err := PutJob(ctx, db, kind.job, key, itemPayload{CaseID: caseID, ItemID: id}); err != nil {
				return err
			}
		}
		return nil
	}
	return replaceJob(ctx, db, JobFinishNormalize, caseID, casePayload{CaseID: caseID})
}

// CancelCase - один путь для «Отмены», /cancel и «начать заново»: слот
// активного обращения свободен, медиа удалено, работы погашены.
//
// Отмена из publishing запрещена: работа publish уже в очереди, и её гашение
// разошлось бы с ответом GitHub - issue создался бы по отменённому обращению.
func (c *Cases) CancelCase(ctx context.Context, cs *Case, reason string) error {
	if cs.Status == statusCancelled || cs.Status == statusPublished || cs.Status == statusAnswered {
		return nil
	}
	if cs.Status == statusPublishing {
		// Лог обязателен: без него нажатая и молча отклонённая кнопка выглядит
		// как зависший бот, а в логах не остаётся ничего.
		c.log.Info("cancel_refused", "case_id", cs.ID, "user_id", cs.UserID, "reason", reason)
		return ErrPublishing
	}

	cancelled := false
	err := c.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE cases SET status = 'cancelled', updated_at = now()
			WHERE id = $1 AND status NOT IN ('published', 'cancelled', 'publishing', 'answered')`, cs.ID)
		if err != nil {
			return fmt.Errorf("cancel case %s: %w", cs.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		cancelled = true

		if err := dropCaseJobs(ctx, tx, cs.ID, "case cancelled"); err != nil {
			return err
		}
		return addEvent(ctx, tx, cs.ID, "case_cancelled", map[string]any{"reason": reason})
	})
	if err != nil {
		return err
	}
	if !cancelled {
		return nil
	}

	cs.Status = statusCancelled
	c.log.Info("case_cancelled", "user_id", cs.UserID, "case_id", cs.ID, "reason", reason)
	// Файлы отменённого обращения не ждут часового сборщика.
	return c.DropFiles(ctx, cs.ID)
}

// dropCaseJobs гасит работы разговора: закрытое обращение не должно получить ни
// вопроса следующего раунда, ни саммари, ни ответа из документации. Уведомление
// автору не гасим - оно и сообщает ему исход.
func dropCaseJobs(ctx context.Context, db Runner, caseID, reason string) error {
	_, err := db.Exec(ctx, `
		UPDATE jobs SET status = 'failed', locked_at = NULL, last_error = $3, updated_at = now()
		WHERE status = 'pending' AND kind = ANY($2)
		  AND payload->>'case_id' = $1`, caseID, caseJobKinds, reason)
	if err != nil {
		return fmt.Errorf("drop jobs of case %s: %w", caseID, err)
	}
	return nil
}

// EndAsk закрывает разговор полученным ответом: слот активного обращения
// свободен, медиа удалено, работы погашены. Отличается от CancelCase причиной, а
// не механикой: cancelled - автор отказался, answered - ответ получен, и разница
// видна владельцу в диагностике.
func (c *Cases) EndAsk(ctx context.Context, cs *Case) error {
	ended := false
	err := c.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE cases SET status = 'answered', updated_at = now()
			WHERE id = $1 AND mode = 'ask'
			  AND status NOT IN ('published', 'cancelled', 'answered')`, cs.ID)
		if err != nil {
			return fmt.Errorf("end ask of case %s: %w", cs.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		ended = true

		if err := dropCaseJobs(ctx, tx, cs.ID, "ask ended"); err != nil {
			return err
		}
		return addEvent(ctx, tx, cs.ID, "ask_ended", nil)
	})
	if err != nil {
		return err
	}
	if !ended {
		return nil
	}

	cs.Status = statusAnswered
	c.log.Info("ask_ended", "user_id", cs.UserID, "case_id", cs.ID)
	return c.DropFiles(ctx, cs.ID)
}

// SwitchToTicket переводит разговор в тикет по кнопке автора. Второе значение -
// переход состоялся: из разбора его не берут, и ни лог, ни ответ автору не
// должны обещать того, чего не было. Состояние перечитывается: перевести мог и
// ход lookup, распознавший просьбу о правке.
func (c *Cases) SwitchToTicket(ctx context.Context, cs *Case) (bool, error) {
	err := c.inTx(ctx, func(tx pgx.Tx) error {
		return switchToTicket(ctx, tx, cs.ID)
	})
	if err != nil {
		return false, err
	}

	fresh, err := c.Load(ctx, cs.ID)
	if err != nil {
		return false, err
	}
	if fresh == nil {
		return false, fmt.Errorf("case %s is gone", cs.ID)
	}
	was := cs.Mode
	cs.Mode, cs.Status = fresh.Mode, fresh.Status
	if was != modeAsk || cs.Mode != modeTicket {
		return false, nil
	}
	c.log.Info("switched_to_ticket", "user_id", cs.UserID, "case_id", cs.ID, "status", cs.Status)
	return true, nil
}

// switchToTicket - переход режима из чужой транзакции: статус, журнал и работы
// живут или не живут вместе. Условие на режим и статус внутри UPDATE: разговор,
// ушедший дальше или занятый разбором, перевод не трогает.
func switchToTicket(ctx context.Context, db txRunner, caseID string) error {
	var status, protocol string
	// Строка блокируется до конца транзакции: между выбором следующего шага и
	// сменой статуса разговор не должен уехать.
	err := db.QueryRow(ctx, `SELECT status, protocol FROM cases WHERE id = $1 FOR UPDATE`,
		caseID).Scan(&status, &protocol)
	if err != nil {
		return fmt.Errorf("read case %s: %w", caseID, err)
	}

	next := statusInterview
	if status == statusCollecting {
		fresh, err := hasNewRaw(ctx, db, caseID, protocol)
		if err != nil {
			return err
		}
		if fresh {
			next = statusNormalizing
		}
	}

	tag, err := db.Exec(ctx, `
		UPDATE cases SET mode = 'ticket', status = $2, updated_at = now()
		WHERE id = $1 AND mode = 'ask' AND status IN ('collecting', 'answering')`, caseID, next)
	if err != nil {
		return fmt.Errorf("switch case %s to ticket: %w", caseID, err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if err := addEvent(ctx, db, caseID, "switched_to_ticket", nil); err != nil {
		return err
	}
	// Сообщение снимает панель сбора одинаково на обоих путях перехода - и по
	// кнопке, и по реплике, распознанной ходом lookup: в разборе «Готово» с этой
	// панели ушло бы ответом автора, а не командой.
	if err := putNotifyKey(ctx, db, caseID, "to-ticket",
		"Что бы вы хотели изменить?", keysHome); err != nil {
		return err
	}
	if next == statusNormalizing {
		// Интервью поставит закрытие нормализации: в режиме тикета это её
		// обычный исход, и своей развилки здесь не нужно.
		return advanceNormalize(ctx, db, caseID)
	}
	return replaceJob(ctx, db, JobInterview, caseID, casePayload{CaseID: caseID})
}

// BuildProtocol склеивает сырьё в один текст - вход медиа-модели на скриншотах
// и диалоговой модели в M2.
//
// Неразобранный скриншот в протокол не попадает: его дописывает
// RunNormalizeImages, который этот протокол и строит.
func BuildProtocol(items []Item) string {
	var b strings.Builder
	n := 0
	for _, it := range items {
		line := itemLine(it)
		if line == "" {
			continue
		}
		n++
		fmt.Fprintf(&b, "%d. %s\n", n, line)
	}
	return strings.TrimRight(b.String(), "\n")
}

var itemLabel = map[string]string{
	"text":  "текст",
	"link":  "ссылка",
	"voice": "голосовое",
	"photo": "скриншот",
}

func itemLine(it Item) string {
	label := itemLabel[it.Kind]
	if label == "" {
		label = it.Kind
	}
	if it.Forwarded {
		label += ", переслано (не слова автора)"
	}

	if it.Status == "failed" {
		reason := strings.TrimSpace(it.Error)
		if reason == "" {
			reason = "причина неизвестна"
		}
		// Провал виден строкой, а не пропуском: модель должна видеть пробел, а
		// не достраивать его сама.
		return label + ": не удалось разобрать: " + oneLine(reason)
	}

	body := strings.TrimSpace(it.Normalized)
	caption := strings.TrimSpace(it.SourceText)
	if body == "" {
		body = caption
		caption = ""
	}
	if body == "" {
		return ""
	}
	// Подпись под пересланным медиа автор набирает сам, поэтому она идёт
	// отдельной строкой и как его слова: пометка «не слова автора» относится к
	// содержимому элемента, а не к тому, что автор написал под ним.
	if caption != "" {
		return label + ": " + body + "\n   слова автора: " + caption
	}
	return label + ": " + body
}

// linkRe - адрес в тексте сообщения. Скобки и угловые исключены нарочно: ссылка
// часто стоит внутри фразы, а хвостовая пунктуация обрезается отдельно.
var linkRe = regexp.MustCompile(`https?://[^\s<>()]+`)

// collectLinks - адреса из сырья обращения в порядке появления, без дублей.
// Собираются кодом, а не моделью: адрес обязан попасть в тикет символ в символ,
// а модель законно сокращает и переписывает текст.
func collectLinks(items []Item) []string {
	var links []string
	seen := map[string]bool{}
	for _, it := range items {
		for _, link := range linkRe.FindAllString(it.SourceText, -1) {
			link = strings.TrimRight(link, ".,;:!?»\"'")
			if link == "" || seen[link] {
				continue
			}
			seen[link] = true
			links = append(links, link)
		}
	}
	return links
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// DropFiles стирает медиа обращения. Сначала обнуляется file_path: путь в БД,
// указывающий на стёртый файл, - ловушка для следующего шага, который решит,
// что файл ещё есть, а забытый файл живёт максимум до перезапуска tmpfs.
func (c *Cases) DropFiles(ctx context.Context, caseID string) error {
	_, err := c.pool.Exec(ctx, `
		UPDATE case_items SET file_path = NULL, updated_at = now()
		WHERE case_id = $1 AND file_path IS NOT NULL`, caseID)
	if err != nil {
		return fmt.Errorf("clear file paths of case %s: %w", caseID, err)
	}
	if err := c.media.Drop(caseID); err != nil {
		return fmt.Errorf("drop files of case %s: %w", caseID, err)
	}
	return nil
}

// SweepDrafts убирает файлы брошенных черновиков и закрытых обращений; статус
// не трогает: автор вернётся к тексту, но не к файлам. Выбор статусов и сроков
// разобран в разделе 5 architecture.md.
func (c *Cases) SweepDrafts(ctx context.Context) error {
	rows, err := c.pool.Query(ctx, `
		SELECT DISTINCT c.id FROM cases c
		JOIN case_items i ON i.case_id = c.id
		WHERE i.file_path IS NOT NULL
		  AND (c.status IN ('published', 'cancelled', 'answered')
		       OR (c.status IN ('collecting', 'interview', 'summary')
		           AND c.updated_at < now() - $1::int * interval '1 second'))`,
		int(draftTTL.Seconds()))
	if err != nil {
		return fmt.Errorf("query stale drafts: %w", err)
	}

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan stale draft: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read stale drafts: %w", err)
	}

	for _, id := range ids {
		// Одно неудачное обращение не отменяет уборку остальных.
		if err := c.DropFiles(ctx, id); err != nil {
			c.log.Error("sweep_failed", "case_id", id, "error", err)
		}
	}
	return nil
}

// RecoverStuck возвращает в очередь работы обращений, которые двигаются только
// работой и остались без неё: такое обращение висит молча, а из publishing не
// отменяют. Причины и границы восстановления - раздел 5 architecture.md.
func (c *Cases) RecoverStuck(ctx context.Context) error {
	rows, err := c.pool.Query(ctx, `
		SELECT c.id, c.status FROM cases c
		WHERE c.status IN ('normalizing', 'publishing', 'answering')
		  AND NOT EXISTS (
		      SELECT 1 FROM jobs j
		      WHERE j.status IN ('pending', 'running')
		        AND j.payload->>'case_id' = c.id::text)`)
	if err != nil {
		return fmt.Errorf("query stuck cases: %w", err)
	}

	type stuck struct{ id, status string }
	var cases []stuck
	for rows.Next() {
		var s stuck
		if err := rows.Scan(&s.id, &s.status); err != nil {
			rows.Close()
			return fmt.Errorf("scan stuck case: %w", err)
		}
		cases = append(cases, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read stuck cases: %w", err)
	}

	for _, s := range cases {
		// Нормализация восстанавливается тем же выбором шага, что и в обычном
		// ходе: сорвалась она на записи, на скриншоте или перед закрытием -
		// решают статусы элементов, а не догадка о том, где всё встало.
		recover := func() error {
			switch s.status {
			case statusNormalizing:
				return advanceNormalize(ctx, c.pool, s.id)
			case statusAnswering:
				return replaceJob(ctx, c.pool, JobLookup, s.id, casePayload{CaseID: s.id})
			}
			return replaceJob(ctx, c.pool, JobPublish, s.id, casePayload{CaseID: s.id})
		}
		if err := recover(); err != nil {
			c.log.Error("recover_failed", "case_id", s.id, "status", s.status, "error", err)
			continue
		}
		c.log.Warn("case_recovered", "case_id", s.id, "status", s.status)
	}
	return nil
}

// RemindDrafts напоминает про обращение, брошенное на сутки. Ровно один раз:
// признак - отметка reminded_at, и второго напоминания не будет, потому что
// бот, пишущий раз в сутки, из помощника становится спамом.
//
// Разговор режима ask напоминания не получает вовсе: вопрос, на который автору
// уже ответили, не черновик и доводить его не надо.
func (c *Cases) RemindDrafts(ctx context.Context) error {
	rows, err := c.pool.Query(ctx, `
		UPDATE cases SET reminded_at = now()
		WHERE status IN ('collecting', 'interview', 'summary')
		  AND mode = 'ticket'
		  AND reminded_at IS NULL
		  AND updated_at < now() - $1::int * interval '1 second'
		RETURNING id, status`, int(draftTTL.Seconds()))
	if err != nil {
		return fmt.Errorf("query abandoned cases: %w", err)
	}

	type draft struct{ id, status string }
	var drafts []draft
	for rows.Next() {
		var d draft
		if err := rows.Scan(&d.id, &d.status); err != nil {
			rows.Close()
			return fmt.Errorf("scan abandoned case: %w", err)
		}
		drafts = append(drafts, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read abandoned cases: %w", err)
	}

	for _, d := range drafts {
		// Отметка уже стоит: не поставив уведомление, мы теряем именно это
		// напоминание, а не блокируем обращение. Ошибка одного не отменяет
		// остальных.
		if err := putNotifyKey(ctx, c.pool, d.id, "remind", remindText(d.status), ""); err != nil {
			c.log.Error("remind_failed", "case_id", d.id, "error", err)
			continue
		}
		c.log.Info("case_reminded", "case_id", d.id, "status", d.status)
	}
	return nil
}

func remindText(status string) string {
	if status == statusCollecting {
		return "Обращение ждёт вас сутки. Пришлите остальное и нажмите «Готово» " +
			"либо нажмите «Сброс». Вложения уже удалены, текст на месте."
	}
	return "Обращение ждёт вашего ответа сутки. Ответьте, и я доведу его до тикета, " +
		"либо нажмите «Сброс». Вложения уже удалены, разбор на месте."
}

type casePayload struct {
	CaseID string `json:"case_id"`
}

type itemPayload struct {
	CaseID string `json:"case_id"`
	ItemID int64  `json:"item_id"`
}

// notifyPayload - сообщение автору, порождённое фоновой работой. Buttons
// называет набор кнопок, а не рисует их: разметку строит bot.go, и он же знает
// текущий раунд.
// Непустой ChatID означает уведомление владельцу: адресат назван явно, кнопок и
// экранов у него нет.
type notifyPayload struct {
	CaseID  string `json:"case_id"`
	Text    string `json:"text"`
	Buttons string `json:"buttons,omitempty"`
	ChatID  int64  `json:"chat_id,omitempty"`
}

// Наборы кнопок под сообщением из очереди.
const (
	keysRound   = "round"   // «Всё так» на раунд вопросов
	keysAsk     = "ask"     // раунд без догадок: подтверждать нечего, кнопки нет
	keysSummary = "summary" // «Публикую», «Поправить»
	keysHome    = "home"    // панель «Меню | Сброс»: обращение доиграно
	keysCancel  = "cancel"  // исход отмены тикета: правит экран, а не шлёт новое
	keysAnswer  = "answer"  // ответ из документации: «Создать тикет», «Закончить разговор»
	keysTicket  = "ticket"  // новость по тикету: одна кнопка перехода в карточку
)

// HandleFailedJob - исход работы, исчерпавшей повторы; воркер зовёт её через
// onFail, сама очередь не знает, что делать с обращением. Гашение работы и
// исход провала - одна транзакция; разбор почему - раздел 5 architecture.md.
func (c *Cases) HandleFailedJob(ctx context.Context, job Job, cause error) {
	var p itemPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.CaseID == "" {
		c.log.Error("job_failed_unlinked", "kind", job.Kind, "job_id", job.ID, "error", err)
		// Работу гасим всё равно: иначе она крутится в очереди вечно.
		if _, err := FailJob(ctx, c.pool, job, cause); err != nil {
			c.log.Error("job_fail_failed", "kind", job.Kind, "job_id", job.ID, "error", err)
		}
		return
	}

	reopened := false
	err := c.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := FailJob(ctx, tx, job, cause); err != nil {
			return err
		}
		if p.ItemID != 0 {
			if err := failItem(ctx, tx, p.ItemID, oneLine(cause.Error())); err != nil {
				return err
			}
		}

		switch job.Kind {
		case JobNormalizeVoice, JobNormalizeImage:
			// Элемент погашен выше, цепочка идёт дальше: провал одной записи или
			// одного экрана не отменяет остального сырья.
			if err := advanceNormalize(ctx, tx, p.CaseID); err != nil {
				return err
			}
		case JobFinishNormalize:
			var err error
			reopened, err = reopenCase(ctx, tx, p.CaseID)
			if err != nil {
				return err
			}
			// Автору говорим только здесь: провал одного голосового виден ему
			// строкой протокола, а вот вставшую цепочку заметить нечем.
			if err := putNotify(ctx, tx, p.CaseID, job.ID,
				"Не смог обработать обращение. Пришлите материал иначе и нажмите «Готово» ещё раз."); err != nil {
				return err
			}
		case JobLookup:
			// Разговор возвращается в сбор: работы не осталось, и без возврата он
			// висел бы в answering молча. Следующая реплика автора поставит новый
			// поход в документацию.
			if _, err := tx.Exec(ctx, `
				UPDATE cases SET status = 'collecting', updated_at = now()
				WHERE id = $1 AND status = 'answering'`, p.CaseID); err != nil {
				return fmt.Errorf("return case %s to collecting: %w", p.CaseID, err)
			}
			// Кнопки те же, что под удачным ответом: оба текста отказа зовут
			// «Создать тикет», а панель сбора её не даёт. При отказе по правам
			// это единственный выход - разговор в документацию не вернётся.
			if err := putNotifyKey(ctx, tx, p.CaseID, strconv.FormatInt(job.ID, 10),
				lookupFailedText(cause), keysAnswer); err != nil {
				return err
			}
		case JobInterview, JobSummarize:
			// Обращение остаётся живым: следующий ответ автора поставит новую
			// работу, и разговор продолжится с того же места.
			if err := putNotify(ctx, tx, p.CaseID, job.ID,
				"Не смог разобрать обращение. Напишите ещё раз своими словами - или нажмите «Сброс»."); err != nil {
				return err
			}
		case JobPublish:
			// Возврат в summary возвращает и кнопку «Публикую» тем же сообщением:
			// тикет не создан, и повторить должен автор, а не повторы очереди.
			if _, err := tx.Exec(ctx, `
				UPDATE cases SET status = 'summary', updated_at = now()
				WHERE id = $1 AND status = 'publishing'`, p.CaseID); err != nil {
				return fmt.Errorf("return case %s to summary: %w", p.CaseID, err)
			}
			if err := putNotifyKey(ctx, tx, p.CaseID, strconv.FormatInt(job.ID, 10),
				publishFailedText(cause), keysSummary); err != nil {
				return err
			}
		case JobNotify:
			// Сообщение автору доставить не удалось, и следа у него нет: вопрос
			// раунда или номер тикета исчезли бы вместе с работой. Владелец
			// получает текст потери и разбирается вручную.
			// Алерт, потерянный сам, второго алерта не порождает: недоступный
			// Telegram кормил бы очередь собственными провалами.
			var n notifyPayload
			if err := json.Unmarshal(job.Payload, &n); err != nil {
				return fmt.Errorf("payload of %s: %w", job.Kind, err)
			}
			if n.ChatID == 0 && c.alertChat != 0 {
				if err := putAlert(ctx, tx, p.CaseID, "lost:"+strconv.FormatInt(job.ID, 10),
					lostNotifyText(p.CaseID, n.Text), c.alertChat); err != nil {
					return err
				}
			}
		case JobCancelIssue:
			// Автору уже ответили «отменяю», и молчание оставило бы его в
			// уверенности, что тикет закрыт: метка могла успеть встать, а
			// закрытие - нет. Повтор за человеком, кнопка на месте. Исход идёт
			// тем же ключом, что и успех: он правит экран отмены, а не копится
			// в памяти бота непрочитанным.
			if err := putNotifyKey(ctx, tx, p.CaseID, strconv.FormatInt(job.ID, 10),
				"Отменить тикет не получилось. Откройте его в списке и попробуйте ещё раз.",
				keysCancel); err != nil {
				return err
			}
		}

		return addEvent(ctx, tx, p.CaseID, "job_failed", map[string]any{
			"kind": job.Kind, "error": oneLine(cause.Error()),
		})
	})
	if err != nil {
		c.log.Error("job_fail_outcome_failed", "kind", job.Kind, "case_id", p.CaseID, "error", err)
		return
	}
	if reopened {
		c.log.Warn("case_reopened", "case_id", p.CaseID)
	}
	if job.Kind == JobCancelIssue {
		c.log.Warn("cancel_failed", "case_id", p.CaseID, "error", oneLine(cause.Error()))
	}
	if job.Kind == JobNotify {
		c.log.Error("notify_lost", "case_id", p.CaseID, "job_id", job.ID,
			"error", oneLine(cause.Error()))
	}
}

// lostNotifyText - шапка алерта о недоставленном сообщении. Текст потери идёт
// целиком: владелец должен видеть, что именно не дошло до автора.
func lostNotifyText(caseID, text string) string {
	return "Сообщение автору не доставлено, обращение " + caseID + ":\n\n" + text
}

// reopenCase возвращает обращение в сбор. Два пути возврата - «разобрать не
// удалось ничего» и провал работы normalize_images - это один переход.
func reopenCase(ctx context.Context, db Runner, caseID string) (bool, error) {
	tag, err := db.Exec(ctx, `
		UPDATE cases SET status = 'collecting', updated_at = now()
		WHERE id = $1 AND status = 'normalizing'`, caseID)
	if err != nil {
		return false, fmt.Errorf("reopen case %s: %w", caseID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// putNotify ставит сообщение автору. Ключ по идентификатору работы: её повтор
// не наплодит вторых уведомлений.
func putNotify(ctx context.Context, db Runner, caseID string, jobID int64, text string) error {
	return putNotifyKey(ctx, db, caseID, strconv.FormatInt(jobID, 10), text, "")
}

// putNotifyKey - то же с явным суффиксом ключа: у напоминания и у раунда
// вопросов нет породившей работы, но повторяться они не должны.
func putNotifyKey(ctx context.Context, db Runner, caseID, suffix, text, buttons string) error {
	key := fmt.Sprintf("%s:%s:%s", JobNotify, caseID, suffix)
	return PutJob(ctx, db, JobNotify, key, notifyPayload{CaseID: caseID, Text: text, Buttons: buttons})
}

// putAlert ставит уведомление владельцу. Работа того же вида, что и сообщение
// автору: механика захвата, повторов и идемпотентности у них совпадает, а
// адресата называет ChatID.
func putAlert(ctx context.Context, db Runner, caseID, suffix, text string, chatID int64) error {
	key := fmt.Sprintf("%s:%s:%s", JobNotify, caseID, suffix)
	return PutJob(ctx, db, JobNotify, key, notifyPayload{CaseID: caseID, Text: text, ChatID: chatID})
}

// replaceJob - единственный способ поставить работу обращения: работа каждого
// вида существует в единственном экземпляре, новая заменяет прежнюю. Следствия
// и защита от внешнего дубля - раздел 4 architecture.md.
func replaceJob(ctx context.Context, db Runner, kind, caseID string, payload any) error {
	if _, err := db.Exec(ctx, `
		DELETE FROM jobs WHERE kind = $2 AND payload->>'case_id' = $1`, caseID, kind); err != nil {
		return fmt.Errorf("clear %s jobs of case %s: %w", kind, caseID, err)
	}
	return PutJob(ctx, db, kind, kind+":"+caseID, payload)
}

// SaveNormalized - переход, который оставляет после себя разбор одного
// элемента. Живёт здесь: единственный владелец состояния обращения - этот файл,
// normalize.go зовёт его, а не пишет свой SQL. Протокол в базу кладёт шаг
// закрытия нормализации, он же переводит обращение в интервью.
func (c *Cases) SaveNormalized(ctx context.Context, itemID int64, text string) error {
	_, err := c.pool.Exec(ctx, `
		UPDATE case_items SET normalized = $2, status = 'done', updated_at = now()
		WHERE id = $1 AND status = 'pending'`, itemID, text)
	if err != nil {
		return fmt.Errorf("save normalized item %d: %w", itemID, err)
	}
	return nil
}

func addEvent(ctx context.Context, db Runner, caseID, kind string, payload any) error {
	raw := []byte("{}")
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal payload of event %s: %w", kind, err)
		}
		raw = encoded
	}

	_, err := db.Exec(ctx, `
		INSERT INTO case_events (case_id, kind, payload) VALUES ($1, $2, $3)`, caseID, kind, raw)
	if err != nil {
		return fmt.Errorf("add event %s: %w", kind, err)
	}
	return nil
}

// inTx держит инвариант среза: переход статуса, событие и постановка работ
// живут или не живут вместе.
func (c *Cases) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return inTx(ctx, c.pool, fn)
}
