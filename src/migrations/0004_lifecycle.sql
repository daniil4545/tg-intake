-- +goose Up

-- Метки статусов заводятся впервые: bootstrap уже отработал в M2 и поднял флаг,
-- иначе новые метки не доехали бы до существующих проектов.
UPDATE projects SET labels_ready = false;

-- Карточка ищет обращение по паре проект-номер: единственность обязана быть
-- гарантией схемы, а не удачей.
CREATE UNIQUE INDEX cases_project_issue ON cases (project_id, issue_number)
    WHERE issue_number IS NOT NULL;

ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check,
                 ADD  CONSTRAINT jobs_kind_check CHECK (kind IN (
                     'normalize_voice', 'normalize_images', 'interview',
                     'summarize', 'publish', 'notify', 'remind', 'cancel_issue'));

-- +goose Down

-- Удаление строго до пересоздания CHECK: ADD CONSTRAINT валидирует таблицу
-- немедленно, и одна строка cancel_issue уронила бы откат.
DELETE FROM jobs WHERE kind = 'cancel_issue';
ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check,
                 ADD  CONSTRAINT jobs_kind_check CHECK (kind IN (
                     'normalize_voice', 'normalize_images', 'interview',
                     'summarize', 'publish', 'notify', 'remind'));
DROP INDEX cases_project_issue;
