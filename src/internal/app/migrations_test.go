package app

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Обе стороны миграции: down-to 0, а не down - down откатывает ровно одну.
// DSN из TEST_DATABASE_URL, не из DATABASE_URL: тест дропает все таблицы, а
// DATABASE_URL молча подхватывается из .env и может смотреть в dev-контур.
func TestMigrations(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}

	const dir = "../../migrations"
	if err := goose.Up(db, dir); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := goose.DownTo(db, dir, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
	if err := goose.Up(db, dir); err != nil {
		t.Fatalf("up after down: %v", err)
	}
}
