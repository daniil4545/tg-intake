# План M0: каркас сервиса

Срез первый. Источник требований - [prd.md](../prd.md), технические решения -
[architecture.md](../architecture.md). Спека не спорит с ними: расхождение
означает возврат к архитектуре, а не правку на месте.

## 1. Что делаем и зачем

Вертикальный срез от апдейта Telegram до строки в PostgreSQL и обратно: бот
запускается, узнаёт автора по белому списку, показывает список проектов из базы
и отвечает выбором. Приёма обращений в этом срезе нет.

Зачем именно так, а не «сначала вся схема и модели»: срез должен доказать всю
цепочку доставки целиком - код, миграции, образ, dev-контур, автодеплой. Пока
цепочка не доказана, любая функциональность сверху пишется вслепую.

Границы среза:

| Входит | Не входит |
| --- | --- |
| Конфиг из окружения с валидацией на старте | Приём сырья, нормализация, интервью |
| Полная схема БД одной миграцией | Очередь `jobs` в работе (таблица есть, воркера нет) |
| Белый список Telegram ID | Клиенты OpenRouter и GitHub |
| `/start`, список проектов, выбор проекта | Кнопки действий над проектом |
| JSON-логи, healthcheck-подкоманда | Уведомления, напоминания |
| Dockerfile, dev compose, CI, автодеплой в dev | Прод-контур |

Схема заводится целиком одной миграцией, хотя срез использует две таблицы из
семи. Причина: схема спроектирована как одно целое и относится к Core; дробить
её на три миграции - это три шанса разойтись с архитектурой ради экономии,
которой нет.

## 2. Структура файлов

Модуль `github.com/daniil4545/tg-intake`, каталог проекта остаётся `intake`.

```text
src/
├── cmd/intake/main.go          # сборка зависимостей, запуск, остановка
├── internal/app/
│   ├── config.go               # чтение и валидация окружения
│   ├── db.go                   # пул, запросы к projects и users
│   └── bot.go                  # telebot, белый список, хендлеры
├── migrations/0001_init.sql
├── Dockerfile
├── docker-compose.yml          # локальный postgres, креды захардкожены
├── Makefile
├── .env.example                # только плейсхолдеры
└── go.mod
deploy/dev/docker-compose.coolify.yml
.github/workflows/{ci.yml,deploy-dev.yml}
```

Файлов ровно столько, сколько ответственностей. `log.go` не заводим: настройка
`slog` - это шесть строк в `main.go`.

`.env.example` лежит рядом с Makefile, потому что тот читает `.env` из своего
каталога. В примере только плейсхолдеры: ни одного настоящего Telegram ID, ни
одного токена. Локальный `docker-compose.yml` поднимает только postgres с
кредами `intake/intake` прямо в файле - это локальная база на ноутбуке, секрета
в ней нет, зато нет и зависимости от `.env`.

## 3. Конфиг

Структура `Config`, читается один раз в `main`, дальше передаётся значением.

| Поле | Переменная | Обязательна | Назначение |
| --- | --- | --- | --- |
| `DatabaseURL` | `DATABASE_URL` | да | строка подключения pgx |
| `BotToken` | `TELEGRAM_BOT_TOKEN` | да | токен Bot API |
| `AllowedIDs` | `TELEGRAM_ALLOWED_IDS` | да | белый список, `int64` через запятую |
| `LogLevel` | `LOG_LEVEL` | нет, `info` | уровень `slog` |
| `Env` | `ENV` | нет, `dev` | попадает в каждую строку лога |

Ключей OpenRouter и GitHub в конфиге нет: срез их не использует, а переменная
без потребителя - это ложное требование к деплою.

```
LoadConfig() (Config, error)
  1. прочитать переменные, обязательные проверить на пустоту
  2. TELEGRAM_ALLOWED_IDS: split по запятой, trim, ParseInt каждого элемента
  3. пустой список - ошибка: бот без белого списка не должен подниматься
  4. вернуть Config либо ошибку, перечисляющую все проблемы конфига разом
     (и незаполненные переменные, и неразобранные значения)
```

Все проблемы разом, а не первая: иначе поднятие контура идёт циклом «запустил,
узнал про одну переменную, запустил снова».

## 4. Схема данных

Миграция `0001_init.sql`, goose, обе стороны `Up` и `Down`. `Down` дропает
таблицы в обратном порядке: без него откат не проверить. Таблицы и назначение
полей - раздел 4 [architecture.md](../architecture.md); здесь фиксируется то,
чего там нет и что иначе придётся угадывать.

Общее: все временные метки `timestamptz NOT NULL DEFAULT now()`, все `jsonb`
c `NOT NULL DEFAULT '{}'` (списки - `'[]'`), уникальные ключи явные.

| Таблица | Ключ и типы | Ограничения |
| --- | --- | --- |
| `projects` | `id bigserial`, `slug text`, `title text`, `github_owner text`, `github_repo text`, `context text`, `labels_ready bool`, `active bool` | `UNIQUE (slug)`; `labels_ready` и `active` с `DEFAULT false` и `true`; `slug` `CHECK (slug ~ '^[a-z0-9-]{1,32}$')` - он уезжает в `callback_data` |
| `users` | `telegram_id bigint PRIMARY KEY`, `first_name text`, `last_name text`, `username text`, `slug text` | `first_name` и `slug` `NOT NULL`; `last_name`, `username` - `NULL` (в Telegram опциональны) |
| `cases` | `id uuid DEFAULT gen_random_uuid()`, `user_id bigint`, `project_id bigint`, `status text`, `kind text`, `protocol text`, `contract jsonb`, `gaps jsonb`, `round int`, `title text`, `summary text`, `incomplete bool`, `issue_number int`, `issue_url text`, `reminded_at timestamptz` | FK на `users(telegram_id)` и `projects(id)`, оба `ON DELETE RESTRICT`; `round DEFAULT 0`; `status CHECK` по семи значениям; `kind CHECK (kind IN ('bug','feature','question'))` и `NULL` до интервью; частичный уникальный индекс, см. ниже |
| `case_items` | `id bigserial`, `case_id uuid`, `kind text`, `tg_message_id bigint`, `tg_file_id text`, `tg_group_id text`, `source_text text`, `normalized text`, `file_path text`, `status text`, `error text` | FK на `cases(id) ON DELETE CASCADE`; `kind CHECK IN ('text','voice','photo','link')`; `status CHECK IN ('pending','done','failed') DEFAULT 'pending'`; индекс `(case_id, status)` |
| `case_events` | `id bigserial`, `case_id uuid`, `kind text`, `payload jsonb` | FK на `cases(id) ON DELETE CASCADE`; индекс `(case_id, created_at)` |
| `jobs` | `id bigserial`, `kind text`, `key text`, `payload jsonb`, `status text`, `attempts int`, `run_after timestamptz`, `locked_at timestamptz`, `last_error text` | `UNIQUE (key)`; `kind CHECK` по семи значениям из архитектуры; `status CHECK IN ('pending','running','done','failed') DEFAULT 'pending'`; `attempts DEFAULT 0`; индекс `(status, run_after)` под захват |

Семь статусов обращения, буквально: `collecting`, `normalizing`, `interview`,
`summary`, `publishing`, `published`, `cancelled`. В `architecture.md` рядом со
схемой стоит «шесть состояний» - это опечатка, машина состояний там же
перечисляет семь; правится вместе с этим срезом.

Частичный уникальный индекс: `UNIQUE (user_id) WHERE status NOT IN
('published','cancelled')` - активное обращение у автора ровно одно.

`status` и `kind` хранятся как `text` с `CHECK`, а не enum: добавление значения
в enum требует миграции с блокировкой, `CHECK` правится дешевле. Внешнего ключа
из `jobs` на `cases` нет: работа ссылается на обращение через `payload`, как в
архитектуре, и это позволяет ставить работы, не привязанные к обращению.

Миграция `0002_seed_project.sql` заводит один проект - сам `tg-intake`:
`slug='tg-intake'`, `github_owner='daniil4545'`, `github_repo='tg-intake'`,
`context` в одну строку, `active=true`, `labels_ready=false`. Вставка с
`ON CONFLICT (slug) DO NOTHING`, `Down` удаляет строку по `slug`. Репозитории
внутренних проектов в публичный репозиторий не попадают: они добавляются
`INSERT`-ом в dev-базу руками. Это заодно и dogfooding - замечания по боту едут
в его собственные Issues.

## 5. Функции

```
Open(ctx, url) (*pgxpool.Pool, error)
  1. pgxpool.ParseConfig, MaxConns = 8 (бот, два воркера, запас),
     затем pgxpool.NewWithConfig; pgxpool.New точки настройки не даёт
  2. Ping с таймаутом 5 секунд
  3. ошибка подключения - фатальная: сервис без базы не работает
```

```
ListProjects(ctx, pool) ([]Project, error)
  SELECT slug, title FROM projects WHERE active ORDER BY title
  Project несёт только Slug и Title: больше срезу не нужно
```

```
UpsertUser(ctx, pool, u User) error
  INSERT ... ON CONFLICT (telegram_id) DO UPDATE
  перезаписываются first_name, last_name, username, slug, updated_at
  причина: люди меняют имя в профиле, шапка issue должна брать текущее
```

```
authorSlug(u User) string
  вход: профиль Telegram, включая telegram_id
  1. если username не пуст - нижний регистр, отфильтровать по [a-z0-9-]
  2. иначе транслитерировать first+last, пробелы в дефис, тот же фильтр
  3. если после фильтра пусто (имя из эмодзи или иероглифов) - "user-<id>"
  4. обрезать до 32 символов
  выход: непустая строка из [a-z0-9-]
  назначение: метка author:<slug>; пустая метка сломала бы публикацию в M2,
  поэтому фолбэк на ID обязателен, а не желателен
```

```
NewBot(cfg, pool, log) (*telebot.Bot, error)
  1. telebot.Settings: LongPoller с таймаутом 10 секунд, Verbose=false,
     OnError - обработчик, пишущий в slog
     причина: по умолчанию telebot пишет ошибки через stdlib log мимо JSON,
     а Verbose дампит сырые payload Bot API вместе с текстами сообщений
  2. middleware белого списка на все апдейты
  3. хендлеры: /start и inline-кнопка с Unique="project"
```

```
allow(cfg) telebot.MiddlewareFunc
  1. отправителя нет (c.Sender() == nil) - отказать и остановить обработку
  2. чат не приватный - отказать: в M1 сюда придут скриншоты и саммари,
     групповой чат для них не место
  3. telegram_id не в AllowedIDs - лог access_denied с user_id, ответ
     отказом, стоп
  4. иначе пропустить дальше
  назначение: единственная точка контроля доступа, обойти её хендлером нельзя
```

```
onStart(ctx) error
  1. UpsertUser текущими данными профиля
  2. ListProjects; пустой список - «проекты не заведены», выход
  3. отправить инлайн-клавиатуру: на проект по кнопке
     telebot.InlineButton{Unique: "project", Data: slug}
     причина: telebot маршрутизирует callback по Unique и кодирует данные как
     \f<unique>|<data>; сырое "p:<slug>" штатным механизмом не поймается
  4. лог start с user_id и числом проектов
```

```
onProject(ctx) error
  1. c.Respond() первым делом: иначе у автора висит спиннер
  2. slug = c.Data(); найти проект среди ListProjects
  3. неизвестный или неактивный slug - «проект недоступен», лог warn
  4. ответить «Проект X выбран. Приём обращений - следующий срез»
  5. лог project_selected с user_id и slug
```

```
healthcheck(cfg) int
  подкоманда /app healthcheck: Ping базы с таймаутом 5 секунд, код 0 или 1
  назначение: образ distroless, ни curl, ни shell внутри нет, проверять
  контейнер больше нечем
  осознанное ограничение: пинг базы не видит умерший long-poller и метит
  сервис unhealthy при мигании базы. Живость поллера проверяется в M1, когда
  появится метка времени последнего апдейта
```

`main` собирает всё вместе: конфиг, логгер, пул, бот; `signal.NotifyContext` на
SIGINT и SIGTERM; по сигналу `bot.Stop()` и `pool.Close()`. Версия сборки
приходит через `-ldflags -X main.version=<sha>` и печатается в логе `starting`:
приёмка Coolify сверяет ревизию в рантайме с кандидатом, иначе доказать, что в
контуре крутится именно собранный образ, нечем.

## 6. Логи

`slog` JSON в stdout. Постоянные поля через `With`: `service=intake`, `env`.
События среза: `starting` (version, env), `db_connected`, `bot_started`,
`access_denied` (user_id), `start` (user_id, projects), `project_selected`
(user_id, slug), `shutdown`.

Ни одной строки с именем, username или текстом сообщения: человек в логе - это
`user_id`.

## 7. Docker и деплой

`Dockerfile` - двухстадийный, как в соседних сервисах платформы: сборка в
`golang:1.25` с `CGO_ENABLED=0`, рантайм `gcr.io/distroless/static:nonroot`.
`goose` собирается из исходников в той же стадии с тегом `no_sqlite3` и
`CGO_ENABLED=0`: релизный бинарь goose линкуется с libc, которой в
`distroless/static` нет, и контейнер миграций упал бы с «no such file or
directory». Каталога данных нет: у сервиса нет состояния на диске, кроме
временных медиафайлов, которые появятся в M1.

`deploy/dev/docker-compose.coolify.yml` - стек `postgres -> migrate -> app`:

- `postgres:16` с healthcheck `pg_isready`;
- `migrate` - тот же образ, `command: ["/goose", "-dir", "/migrations", "up"]`,
  DSN только через `GOOSE_DBSTRING` в окружении, не аргументом: аргументы видны
  в `docker inspect` и в UI Coolify. Обязательны `exclude_from_hc: true` и
  `restart: "no"`, иначе завершившийся one-shot контейнер держит сервис
  не-healthy и приёмка не проходит при полностью рабочем коде;
- `app` зависит от `migrate` через `service_completed_successfully`,
  healthcheck `["CMD", "/app", "healthcheck"]`, `restart: unless-stopped`;
- логи в journald с тегом `intake-dev-{{.Name}}`.

Шага `backup` из соседнего сервиса здесь нет: в dev-контуре нет данных, потерю
которых стоит переживать, а лишний one-shot контейнер удлиняет каждый выкат.

CI (`ci.yml`) на PR в `dev` и `main`: сервисный PostgreSQL, одна цель
`make ci-check` (миграции, тесты, vet, build) плюс `docker build` без push.
Список проверок живёт в Makefile в одном экземпляре, workflow его только зовёт.
Сборка образа в CI нужна потому, что иначе первый выкат станет и первой сборкой
Dockerfile, а ошибка многостадийной сборки под distroless вылезет в деплое, а не
в PR.

Автодеплой (`deploy-dev.yml`) на push в `dev`: те же проверки, сборка и push
образа `ghcr.io/daniil4545/tg-intake:sha-<commit>`, синхронизация Compose в
Coolify через `PATCH /api/v1/services/{uuid}`, установка `APP_IMAGE`, запуск
деплоя.

**Отличие от канона соседних сервисов, обязательное для публичного
репозитория.** Адрес Coolify и UUID сервиса не пишутся в workflow открытым
текстом, а берутся из секретов `COOLIFY_API_BASE` и `COOLIFY_DEV_SERVICE_UUID`:
в публичном репозитории это карта внутренней инфраструктуры. Проверка
`github.actor == 'daniil4545'` на job сборки и деплоя сохраняется - в публичном
репозитории она перестаёт быть формальностью.

Секреты репозитория, нужные срезу: `COOLIFY_TOKEN`, `COOLIFY_API_BASE`,
`COOLIFY_DEV_SERVICE_UUID`. `GITHUB_TOKEN` для GHCR выдаёт сам Actions.

## 8. Критерий приёмки

1. `make ci-check` проходит локально и в CI: миграции, тесты, vet, build.
2. `docker compose up` поднимает postgres, миграции выходят с кодом 0,
   контейнер приложения переходит в `healthy`.
3. Автор из белого списка отправляет `/start` и получает список проектов;
   нажатие на проект даёт подтверждение выбора.
4. Автор вне белого списка получает отказ, и в базе не появляется ни строки.
5. В логе есть `bot_started`, `start`, `project_selected`; нет ни одного имени,
   username или текста сообщения.
6. Push в `dev` собирает образ `sha-<commit>` в GHCR и перевыкатывает
   dev-сервис Coolify; после выката `app` в состоянии healthy, а в логе
   `starting` версия равна выкаченному коммиту.

Пункт 6 требует заведённого сервиса Coolify и трёх секретов. Пока их нет, срез
принимается по пунктам 1-5, а деплой проверяется отдельно.

**Один токен - один поллер.** Bot API отдаёт апдейты только одному long-poller:
второй получает 409 Conflict. Поэтому пункты 3 и 6 нельзя проверять
одновременно на одном токене - либо локальный запуск, либо dev-контур. Для
постоянного dev-контура нужен отдельный бот, пока проверяем по очереди.

## 9. Тестовый сценарий

Тесты проверяют поведение, а не реализацию. Их три, и больше не нужно.

| Тест | Что подаём | Что ожидаем |
| --- | --- | --- |
| `TestLoadConfig` | окружение без `DATABASE_URL` и с мусором в `TELEGRAM_ALLOWED_IDS` | ошибка называет обе проблемы; на валидном наборе список ID разобран |
| `TestAuthorSlug` | «Иван Петров» без username, username с заглавными, имя из одних эмодзи | метка из `[a-z0-9-]`, непустая, короче 33 символов; в третьем случае `user-<id>` |
| `TestMigrations` | `goose up`, затем `down-to 0` и снова `up` на чистой базе | обе стороны применяются, схема воспроизводится |

`TestMigrations` берёт DSN из `DATABASE_URL` и пропускается (`t.Skip`), если
переменная пуста: `go test ./...` без базы должен проходить. Он единственный
трогает базу, поэтому гонки с параллельными пакетами нет. `down-to 0`, а не
`down`: `down` откатывает ровно одну миграцию, то есть только seed, и схема
осталась бы непроверенной.

Проверка белого списка тестом не покрывается: это несколько строк в middleware,
и она проверяется пунктом 4 приёмки живьём. Тест на неё был бы тестом на
`slices.Contains`.

## 10. Что останется незакрытым после среза

- Сервис Coolify и три секрета - зависят от владельца.
- Приём обращений: бот отвечает на выбор проекта заглушкой. Это осознанная
  граница среза, а не недоделка.
- Таблицы `cases`, `case_items`, `case_events`, `jobs` заведены, но не
  используются до M1.
- Закрепление `api.telegram.org` за рабочим DC понадобилось соседнему Bot
  API-сервису на этом же VPS. Симптом при промахе - бот healthy и молча без
  апдейтов. Проверяется при первом выкате, решение записывается в базу знаний.
- `CHANGELOG.md` пополняется в том же PR: журнал ведётся с первого среза.
