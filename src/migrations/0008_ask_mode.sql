-- +goose Up
-- Режим разговора: ticket идёт как шёл, ask отвечает по документации проекта.
-- Дефолт делает миграцию безопасной для существующих обращений.
ALTER TABLE cases ADD COLUMN mode text NOT NULL DEFAULT 'ticket'
                  CHECK (mode IN ('ticket', 'ask'));

-- answering - поход в документацию идёт, обращение двигает работа, и его
-- поднимает RecoverStuck. answered - разговор закрыт полученным ответом, в
-- отличие от cancelled, где автор отказался.
ALTER TABLE cases DROP CONSTRAINT cases_status_check,
                  ADD  CONSTRAINT cases_status_check CHECK (status IN (
                      'collecting', 'normalizing', 'interview', 'summary',
                      'publishing', 'published', 'cancelled',
                      'answering', 'answered'));

-- «Закончить разговор» доступен и до того, как проект попал в обращение,
-- поэтому answered встаёт в один ряд с collecting и cancelled.
ALTER TABLE cases DROP CONSTRAINT cases_project_before_work,
                  ADD  CONSTRAINT cases_project_before_work
                  CHECK (status IN ('collecting', 'cancelled', 'answered')
                         OR project_id IS NOT NULL);

-- Слот активного обращения освобождает и закрытый ответом разговор: иначе
-- «Закончить разговор» не даёт автору завести следующее.
DROP INDEX cases_one_active_per_user;
CREATE UNIQUE INDEX cases_one_active_per_user ON cases (user_id)
    WHERE status NOT IN ('published', 'cancelled', 'answered');

ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check,
                 ADD  CONSTRAINT jobs_kind_check
                 CHECK (kind IN ('normalize_voice', 'normalize_image', 'finish_normalize',
                                 'interview', 'summarize', 'publish', 'notify',
                                 'cancel_issue', 'lookup'));

-- +goose Down
-- Удаление строго до пересоздания CHECK и индекса: ADD CONSTRAINT валидирует
-- таблицу немедленно, а частичный уникальный индекс перестал бы сходиться, если
-- у автора закрытый ответом разговор соседствует с живым обращением.
DELETE FROM jobs WHERE kind = 'lookup';
DELETE FROM cases WHERE status IN ('answering', 'answered');

ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check,
                 ADD  CONSTRAINT jobs_kind_check
                 CHECK (kind IN ('normalize_voice', 'normalize_image', 'finish_normalize',
                                 'interview', 'summarize', 'publish', 'notify',
                                 'cancel_issue'));

DROP INDEX cases_one_active_per_user;
CREATE UNIQUE INDEX cases_one_active_per_user ON cases (user_id)
    WHERE status NOT IN ('published', 'cancelled');

ALTER TABLE cases DROP CONSTRAINT cases_project_before_work,
                  ADD  CONSTRAINT cases_project_before_work
                  CHECK (status IN ('collecting', 'cancelled') OR project_id IS NOT NULL);

ALTER TABLE cases DROP CONSTRAINT cases_status_check,
                  ADD  CONSTRAINT cases_status_check CHECK (status IN (
                      'collecting', 'normalizing', 'interview', 'summary',
                      'publishing', 'published', 'cancelled'));

ALTER TABLE cases DROP COLUMN mode;
