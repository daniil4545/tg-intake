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

DELETE FROM projects WHERE slug = 'tg-intake';
