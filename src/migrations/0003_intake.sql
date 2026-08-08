-- +goose Up

ALTER TABLE cases      ALTER COLUMN project_id DROP NOT NULL,
                       ADD CONSTRAINT cases_project_before_work
                       CHECK (status = 'collecting' OR project_id IS NOT NULL);
ALTER TABLE case_items ADD COLUMN forwarded boolean NOT NULL DEFAULT false,
                       ADD COLUMN mime      text;

-- +goose Down

ALTER TABLE case_items DROP COLUMN mime, DROP COLUMN forwarded;
ALTER TABLE cases      DROP CONSTRAINT cases_project_before_work;
UPDATE cases SET status = 'cancelled' WHERE project_id IS NULL;  -- иначе SET NOT NULL упадёт
ALTER TABLE cases      ALTER COLUMN project_id SET NOT NULL;
