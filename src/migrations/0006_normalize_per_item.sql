-- +goose Up
-- Нормализация идёт по элементу: скриншот получает свою работу, как и запись, а
-- закрывает разбор отдельная finish_normalize. Прежний normalize_images держал
-- всю пачку в одном бюджете работы. Мёртвое значение remind уходит той же
-- правкой: напоминания работают через notify с суффиксом ключа.
DELETE FROM jobs WHERE kind IN ('normalize_images', 'remind');

ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check,
                 ADD  CONSTRAINT jobs_kind_check
                 CHECK (kind IN ('normalize_voice', 'normalize_image', 'finish_normalize',
                                 'interview', 'summarize', 'publish', 'notify', 'cancel_issue'));

-- Обращения, оставшиеся без работы, поднимет RecoverStuck первым же стартом.

-- Разовая уборка очереди; дальше отработавшие работы убирает SweepJobs.
DELETE FROM jobs WHERE status = 'done' AND updated_at < now() - interval '7 days';

-- +goose Down
DELETE FROM jobs WHERE kind IN ('normalize_image', 'finish_normalize');

ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check,
                 ADD  CONSTRAINT jobs_kind_check
                 CHECK (kind IN ('normalize_voice', 'normalize_images', 'interview', 'summarize',
                                 'publish', 'notify', 'remind', 'cancel_issue'));
