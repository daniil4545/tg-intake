-- +goose Up

-- Единственный проект в публичном репозитории - сам сервис: замечания по боту
-- едут в его собственные Issues. Внутренние проекты добавляются INSERT-ом в
-- dev-базу и в публичный репозиторий не попадают.
INSERT INTO projects (slug, title, github_owner, github_repo, context)
VALUES (
    'tg-intake',
    'Предложка (сам бот)',
    'daniil4545',
    'tg-intake',
    'Telegram-бот приёма обращений: доводит сырой фидбек интервью до готового описания и заводит тикет в GitHub Issues.'
)
ON CONFLICT (slug) DO NOTHING;

-- +goose Down

-- Условие обязательно: FK cases.project_id стоит на ON DELETE RESTRICT, и на
-- базе, где сервисом уже пользовались, безусловный DELETE ронял бы откат.
-- Строка в этом случае уезжает вместе с таблицей на откате 0001.
DELETE FROM projects p
WHERE p.slug = 'tg-intake'
  AND NOT EXISTS (SELECT 1 FROM cases c WHERE c.project_id = p.id);
