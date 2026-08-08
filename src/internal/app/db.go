package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Project - строка меню. Мультипроектность с первого дня: добавление проекта
// это INSERT, деплой не нужен.
type Project struct {
	Slug  string
	Title string
}

// User - профиль автора из Telegram. ФИО нужно для шапки issue: GitHub не даёт
// создать тикет от чужого имени, поэтому авторство фиксируется телом и меткой.
type User struct {
	ID       int64
	First    string
	Last     string
	Username string
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

func ListProjects(ctx context.Context, pool *pgxpool.Pool) ([]Project, error) {
	rows, err := pool.Query(ctx, `SELECT slug, title FROM projects WHERE active ORDER BY title`)
	if err != nil {
		return nil, fmt.Errorf("query projects: %w", err)
	}
	defer rows.Close()

	var projects []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.Slug, &p.Title); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
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
