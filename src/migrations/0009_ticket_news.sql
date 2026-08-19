-- +goose Up
-- Слежение за тикетом хранит не статус, а факт доставки: единственный источник
-- истины по статусу остаётся меткой issue, и показ по-прежнему читает GitHub.
-- NULL в told_status значит «тикет ещё не наблюдали»: первый тик записывает
-- текущую метку молча, иначе выкат разослал бы новость по каждому старому тикету.
ALTER TABLE cases ADD COLUMN told_status     text,
                  ADD COLUMN told_comment_id bigint  NOT NULL DEFAULT 0,
                  ADD COLUMN has_news        boolean NOT NULL DEFAULT false,
                  ADD COLUMN brief           text;

-- Граница окна опроса комментариев. Дефолт now() отсекает прошлую переписку:
-- на существующих проектах выкат не рассылает историю.
ALTER TABLE projects ADD COLUMN comments_since timestamptz NOT NULL DEFAULT now();

-- +goose Down
ALTER TABLE projects DROP COLUMN comments_since;
ALTER TABLE cases DROP COLUMN told_status,
                  DROP COLUMN told_comment_id,
                  DROP COLUMN has_news,
                  DROP COLUMN brief;
