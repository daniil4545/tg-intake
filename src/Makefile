ifneq ($(NO_DOTENV),1)
    ENV_FILE := $(firstword $(wildcard .env ../.env))
    ifneq (,$(ENV_FILE))
        -include $(ENV_FILE)
        export
    endif
endif

GOOSE := go run -tags='no_sqlite3' github.com/pressly/goose/v3/cmd/goose@v3.27.3
MIGRATIONS_DIR := ./migrations

.PHONY: run build test vet ci-check check-db migrate-up migrate-down migrate-reset docker-up docker-logs docker-down

run:
	go run ./cmd/intake

build:
	go build ./...

# RUN фильтрует тесты при разборе: make test RUN=TestAuthorSlug.
test:
	go test ./... $(if $(RUN),-run $(RUN) -count=1,)

vet:
	go vet ./...

ci-check: migrate-up test vet build

# DATABASE_URL молча подхватывается из .env, поэтому migrate-* может уехать в
# чужую базу. Печатаем цель без пароля и требуем ALLOW_REMOTE=1 для всего, что
# не localhost.
check-db:
	@test -n "$(DATABASE_URL)" || { echo "DATABASE_URL is not set"; exit 1; }
	@rest=$$(printf '%s' "$(DATABASE_URL)" | sed -e 's|^[a-zA-Z][a-zA-Z0-9+.-]*://||' -e 's|^.*@||'); \
	host=$${rest%%[:/?]*}; \
	echo "migration target: host=$${host:-local}"; \
	case "$$host" in \
	localhost|127.0.0.1|::1|"") ;; \
	*) \
		if [ "$(ALLOW_REMOTE)" = "1" ]; then \
			echo "WARNING: remote target $$host, running because ALLOW_REMOTE=1"; \
		else \
			echo "refusing to migrate non-local host $$host; re-run with ALLOW_REMOTE=1 if this is intended"; \
			exit 1; \
		fi ;; \
	esac

# Команды не эхом: в них целиком лежит DATABASE_URL с паролем, а цель миграции
# уже напечатал check-db.
migrate-up: check-db
	@$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" up

migrate-down: check-db
	@$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" down

migrate-reset: check-db
	@$(GOOSE) -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" down-to 0

docker-up:
	docker compose up -d postgres

docker-logs:
	docker compose logs -f --tail=200 postgres

docker-down:
	docker compose down
