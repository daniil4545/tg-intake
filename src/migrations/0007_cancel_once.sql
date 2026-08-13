-- +goose Up
-- Отмена тикета записывается один раз, и держит это база: проверка перед
-- вставкой пропускала второе событие, когда две работы отмены шли
-- одновременно - каждая видела пустой журнал до чужого коммита.
DELETE FROM case_events a USING case_events b
WHERE a.kind = 'cancelled_by_author' AND b.kind = 'cancelled_by_author'
  AND a.case_id = b.case_id AND a.id > b.id;

CREATE UNIQUE INDEX case_events_cancel_once ON case_events (case_id)
WHERE kind = 'cancelled_by_author';

-- +goose Down
DROP INDEX case_events_cancel_once;
