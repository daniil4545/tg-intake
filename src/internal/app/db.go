package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Project - строка меню и адрес публикации разом. Мультипроектность с первого
// дня: добавление проекта это INSERT, деплой не нужен.
//
// Context уходит в системный промт интервью, поэтому обязан быть стабильным:
// правка строки в базе гасит кэш префикса у этого проекта.
type Project struct {
	ID          int64
	Slug        string
	Title       string
	Owner       string
	Repo        string
	Context     string
	LabelsReady bool
	// CommentsSince - граница окна, за которым слежение ищет новые комментарии.
	// Двигается только после успешного разбора: отказ GitHub оставляет её на
	// месте, и следующий тик перекрывает пропущенное.
	CommentsSince time.Time
}

// User - профиль автора из Telegram. ФИО нужно для шапки issue: GitHub не даёт
// создать тикет от чужого имени, поэтому авторство фиксируется телом и меткой.
//
// Slug заполняется только при чтении: на записи его считает authorSlug, и два
// источника одного значения разошлись бы на первом переименовании.
type User struct {
	ID       int64
	First    string
	Last     string
	Username string
	Slug     string
}

func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 8

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

const projectColumns = `id, slug, title, github_owner, github_repo, context, labels_ready,
	comments_since`

func ListProjects(ctx context.Context, pool *pgxpool.Pool) ([]Project, error) {
	rows, err := pool.Query(ctx, `SELECT `+projectColumns+` FROM projects WHERE active ORDER BY title`)
	if err != nil {
		return nil, fmt.Errorf("query projects: %w", err)
	}
	defer rows.Close()

	var projects []Project
	for rows.Next() {
		var p Project
		if err := scanProject(rows, &p); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// LoadProject нужен шагам, идущим из очереди: у них на руках только id из
// обращения.
func LoadProject(ctx context.Context, pool *pgxpool.Pool, id int64) (Project, error) {
	var p Project
	row := pool.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = $1`, id)
	if err := scanProject(row, &p); err != nil {
		return Project{}, err
	}
	return p, nil
}

func scanProject(row pgx.Row, p *Project) error {
	if err := row.Scan(&p.ID, &p.Slug, &p.Title, &p.Owner, &p.Repo, &p.Context, &p.LabelsReady,
		&p.CommentsSince); err != nil {
		return fmt.Errorf("scan project: %w", err)
	}
	return nil
}

// SyncProjects заводит проекты из конфига. Это сид, а не источник истины:
// существующая строка не трогается, иначе рестарт молча откатывал бы правки
// команды /project. labels_ready не трогаем: старт прогоняет PrepareProject
// по всем активным проектам, и метки нового репозитория заведутся сами.
func SyncProjects(ctx context.Context, pool *pgxpool.Pool, list []ProjectConfig, log *slog.Logger) error {
	if len(list) == 0 {
		return nil
	}

	added := 0
	for _, p := range list {
		tag, err := pool.Exec(ctx, `
			INSERT INTO projects (slug, title, github_owner, github_repo, context, active)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (slug) DO NOTHING`,
			p.Slug, p.Title, p.Owner, p.Repo, p.Context, p.IsActive())
		if err != nil {
			return fmt.Errorf("seed project %s: %w", p.Slug, err)
		}
		if tag.RowsAffected() > 0 {
			added++
			log.Info("project_added", "project", p.Slug)
		}
	}

	if added > 0 {
		log.Info("projects_seeded", "added", added, "listed", len(list))
	}
	return nil
}

// MarkLabelsReady снимает bootstrap меток с горячего пути: второй тикет того же
// проекта уже не тратит запросы на создание существующих меток.
func MarkLabelsReady(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	_, err := pool.Exec(ctx, `UPDATE projects SET labels_ready = true, updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("mark labels ready of project %d: %w", id, err)
	}
	return nil
}

// inTx выполняет функцию в транзакции. Живёт здесь, а не методом владельца
// состояния: транзакции нужны и обращению, и слежению, а две копии одного
// шаблона разошлись бы на первой правке.
func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// MoveCommentsSince сдвигает границу окна опроса комментариев. Назад она не
// ходит: тик, разобравший меньше прошлого, не имеет права заставить сервис
// перечитать уже доставленное.
func MoveCommentsSince(ctx context.Context, pool *pgxpool.Pool, id int64, at time.Time) error {
	_, err := pool.Exec(ctx, `
		UPDATE projects SET comments_since = $2, updated_at = now()
		WHERE id = $1 AND comments_since < $2`, id, at)
	if err != nil {
		return fmt.Errorf("move comments border of project %d: %w", id, err)
	}
	return nil
}

// UpsertUser перезаписывает профиль текущими данными: люди меняют имя и
// username, а шапка issue должна брать актуальное.
func UpsertUser(ctx context.Context, pool *pgxpool.Pool, u User) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO users (telegram_id, first_name, last_name, username, slug)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (telegram_id) DO UPDATE SET
			first_name = EXCLUDED.first_name,
			last_name  = EXCLUDED.last_name,
			username   = EXCLUDED.username,
			slug       = EXCLUDED.slug,
			updated_at = now()`,
		u.ID, u.First, nullable(u.Last), nullable(u.Username), authorSlug(u))
	if err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}
	return nil
}

// LoadUser нужен публикации: шапка issue и метка автора берутся из профиля,
// сохранённого на последнем обращении.
func LoadUser(ctx context.Context, pool *pgxpool.Pool, id int64) (User, error) {
	u := User{ID: id}
	err := pool.QueryRow(ctx, `
		SELECT first_name, COALESCE(last_name, ''), COALESCE(username, ''), slug
		FROM users WHERE telegram_id = $1`, id).Scan(&u.First, &u.Last, &u.Username, &u.Slug)
	if err != nil {
		return User{}, fmt.Errorf("load user %d: %w", id, err)
	}
	return u, nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// authorSlug строит значение метки author:<slug>. Пустая метка сломала бы
// публикацию, поэтому у имени из эмодзи или иероглифов есть фолбэк на ID.
func authorSlug(u User) string {
	slug := clean(strings.ToLower(u.Username))
	if slug == "" {
		slug = clean(translit(strings.ToLower(u.First + " " + u.Last)))
	}
	if slug == "" {
		slug = fmt.Sprintf("user-%d", u.ID)
	}
	if len(slug) > 32 {
		slug = strings.Trim(slug[:32], "-")
	}
	return slug
}

// clean оставляет только то, что GitHub примет в имени метки, схлопывая
// подряд идущие разделители в один дефис.
func clean(value string) string {
	var b strings.Builder
	dash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case r == ' ' || r == '-' || r == '_' || r == '.':
			if !dash && b.Len() > 0 {
				b.WriteRune('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

var cyrillic = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e",
	'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m",
	'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "h", 'ц': "c", 'ч': "ch", 'ш': "sh", 'щ': "sch", 'ъ': "",
	'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
}

func translit(value string) string {
	var b strings.Builder
	for _, r := range value {
		if latin, ok := cyrillic[r]; ok {
			b.WriteString(latin)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
