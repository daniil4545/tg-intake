-- +goose Up
-- Живой экран обращения: id последнего сообщения шага, кнопки которого ещё
-- действуют, и раунд, к которому оно относится (0 - счётчик сбора или саммари,
-- это не раунд ответа). bigint - у message_id Bot API нет обещания 32 бит.
ALTER TABLE cases ADD COLUMN screen_msg   bigint  NOT NULL DEFAULT 0,
                  ADD COLUMN screen_round integer NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE cases DROP COLUMN screen_msg,
                  DROP COLUMN screen_round;
