-- +goose Up
-- Отмена не требует проекта: обращение, брошенное до выбора проекта, обязано
-- закрываться. Прежний CHECK разрешал без проекта только collecting, и «Сброс»
-- такого обращения падал - автор не мог ни закрыть его, ни пройти мимо.
ALTER TABLE cases DROP CONSTRAINT cases_project_before_work,
                  ADD  CONSTRAINT cases_project_before_work
                  CHECK (status IN ('collecting', 'cancelled') OR project_id IS NOT NULL);

-- +goose Down
-- NOT VALID обязателен: отменённые без проекта строки уже существуют, и
-- валидный старый CHECK не дал бы откатиться.
ALTER TABLE cases DROP CONSTRAINT cases_project_before_work,
                  ADD  CONSTRAINT cases_project_before_work
                  CHECK (status = 'collecting' OR project_id IS NOT NULL) NOT VALID;
