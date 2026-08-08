-- +goose Up

CREATE TABLE projects (
    id            bigserial PRIMARY KEY,
    slug          text        NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9-]{1,32}$'),
    title         text        NOT NULL,
    github_owner  text        NOT NULL,
    github_repo   text        NOT NULL,
    context       text        NOT NULL DEFAULT '',
    labels_ready  boolean     NOT NULL DEFAULT false,
    active        boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- last_name и username в Telegram опциональны, поэтому NULL допустим.
CREATE TABLE users (
    telegram_id bigint PRIMARY KEY,
    first_name  text        NOT NULL,
    last_name   text,
    username    text,
    slug        text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE cases (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      bigint      NOT NULL REFERENCES users (telegram_id) ON DELETE RESTRICT,
    project_id   bigint      NOT NULL REFERENCES projects (id) ON DELETE RESTRICT,
    status       text        NOT NULL CHECK (status IN (
                     'collecting', 'normalizing', 'interview', 'summary',
                     'publishing', 'published', 'cancelled')),
    kind         text        CHECK (kind IN ('bug', 'feature', 'question')),
    protocol     text        NOT NULL DEFAULT '',
    contract     jsonb       NOT NULL DEFAULT '{}',
    gaps         jsonb       NOT NULL DEFAULT '[]',
    round        integer     NOT NULL DEFAULT 0,
    title        text,
    summary      text,
    incomplete   boolean     NOT NULL DEFAULT false,
    issue_number integer,
    issue_url    text,
    reminded_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Активное обращение у автора ровно одно: иначе появляется класс вопросов
-- «в какой из трёх черновиков попало голосовое».
CREATE UNIQUE INDEX cases_one_active_per_user
    ON cases (user_id)
    WHERE status NOT IN ('published', 'cancelled');

CREATE TABLE case_items (
    id            bigserial PRIMARY KEY,
    case_id       uuid        NOT NULL REFERENCES cases (id) ON DELETE CASCADE,
    kind          text        NOT NULL CHECK (kind IN ('text', 'voice', 'photo', 'link')),
    tg_message_id bigint,
    tg_file_id    text,
    tg_group_id   text,
    source_text   text        NOT NULL DEFAULT '',
    normalized    text        NOT NULL DEFAULT '',
    file_path     text,
    status        text        NOT NULL DEFAULT 'pending'
                              CHECK (status IN ('pending', 'done', 'failed')),
    error         text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX case_items_case_status ON case_items (case_id, status);

CREATE TABLE case_events (
    id         bigserial PRIMARY KEY,
    case_id    uuid        NOT NULL REFERENCES cases (id) ON DELETE CASCADE,
    kind       text        NOT NULL,
    payload    jsonb       NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX case_events_case_created ON case_events (case_id, created_at);

-- Очередь и она же outbox. Внешнего ключа на cases нет: работа ссылается на
-- обращение через payload, и это позволяет ставить работы без обращения.
CREATE TABLE jobs (
    id         bigserial PRIMARY KEY,
    kind       text        NOT NULL CHECK (kind IN (
                   'normalize_voice', 'normalize_images', 'interview',
                   'summarize', 'publish', 'notify', 'remind')),
    key        text        NOT NULL UNIQUE,
    payload    jsonb       NOT NULL DEFAULT '{}',
    status     text        NOT NULL DEFAULT 'pending'
                           CHECK (status IN ('pending', 'running', 'done', 'failed')),
    attempts   integer     NOT NULL DEFAULT 0,
    run_after  timestamptz NOT NULL DEFAULT now(),
    locked_at  timestamptz,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX jobs_claim ON jobs (status, run_after);

-- +goose Down

DROP TABLE jobs;
DROP TABLE case_events;
DROP TABLE case_items;
DROP TABLE cases;
DROP TABLE users;
DROP TABLE projects;
