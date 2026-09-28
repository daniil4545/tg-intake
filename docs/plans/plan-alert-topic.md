# Спека среза: уведомления владельцу в тему группы

Статус: agreed (решения закрыты автором без владельца, список - раздел 2)
Issue: galera-tasks#70, срез 1 из 2 (`alert-topic`)
Architecture review: pending
Каноны: `galera-infra/docs/plans/plan-alert-channel.md` (темы: Coolify 4, intake 5, бэкап 6,
qualifier 7), `docs/contracts.md` (уведомления), Bot API `sendMessage.message_thread_id`

## 1. Цель и границы

- Цель: уведомления intake (новый тикет, отмена, вопрос, потерянное сообщение автору)
  идут `@galera_alert_bot` в группу «Galera Alerts», тема 5, а не старым ботом в личку.
- Готовый результат: при заданной `ALERT_THREAD_ID` `sendMessage` уведомления несёт
  `message_thread_id`; пустая переменная - вызов как сейчас.
- Делаем: переменная, разбор в конфиге, передача в отправку, compose, `.env.example`,
  справочники, раздел «Выкат».
- Не делаем: бэкап и qualifier (срез 2 и отдельные задачи #70), смену текста
  уведомлений, тему для сообщений автору.

## 2. Архитектура

Поток прежний: `putAlert` кладёт работу `notify` с `ChatID` в очередь, `Bot.Notify`
шлёт её ботом `b.alert`. Меняется только последний шаг: `Send` получает
`&tele.SendOptions{ThreadID: b.alertThread}`; telebot пишет `message_thread_id` только при
ненулевом значении (`options.go:219`).

Решения:
- Имя `ALERT_THREAD_ID`: пара к `ALERT_CHAT_ID`, термин Bot API. `ALERT_THREAD_<ИСТОЧНИК>`
  из `galera-infra` там различает источники, у intake источник один.
- Тема - настройка канала, а не события: живёт в `Bot` из конфига, в payload работы не
  пишется. Отказ от поля в `notifyPayload`: четыре места `putAlert` и три конструктора
  ради значения, которое у всех одно. Пересмотр - если появятся уведомления в разные темы.
- Тема ставится только если `p.ChatID == b.alertChat`: работа, поставленная до выката
  в прежний чат (личка), не получит чужую тему и `400 message thread not found`.
- Уровень 1 (не секрет): в `deploy/env-map/intake.txt` не входит, ставится `set-env`.
- Кривое значение (`abc`, `0`, `-3`) - проблема `LoadConfig`, старт падает, как у
  `ALERT_CHAT_ID`. Тема без чата - не ошибка: уведомления выключены целиком.

## 3. Сценарии

| Сценарий | Шаги | Результат |
|---|---|---|
| Happy path | чат и тема 5 заданы, работа с `ChatID` = чату | `sendMessage` с `message_thread_id=5` |
| Без темы | `ALERT_THREAD_ID` пуста | вызов без `message_thread_id`, как до среза |
| Работа в прежний чат | в очереди работа с другим `ChatID` | без темы; новый бот без диалога с владельцем - `403`, провал в `job-errors` (окно - очередь на миг выката) |
| Повтор | работа повторяется очередью | та же тема; дубль исключает ключ работы, как сейчас |
| Тема удалена | Telegram `400` | ошибка `send alert`, повторы и провал по политике очереди, `job-errors` |

## 3a. Рубежи молчания

| Состояние | Чего не делаем | Чем держится | Тест |
|---|---|---|---|
| сообщение автору (`ChatID == 0`) | не шлём с `message_thread_id` | тема только в ветке `p.ChatID != 0` | `TestNotifyAuthorIgnoresAlertThread` |
| работа в чат не из конфига | не ставим тему | сравнение с `b.alertChat` | `TestNotifyAlertThread/other_chat` |

## 3b. Сценарии проверки

Пишет `test-designer`; минимум - тесты 3a и 6.

## 4. Данные и состояния

Схема БД и payload не меняются. Новое - `Config.AlertThreadID int` (0 - без темы) и
поля `Bot.alertChat int64`, `Bot.alertThread int`. Повтор работы идемпотентен как прежде.

## 5. Кодовая модель

```go
type Config struct { /* ... */ AlertThreadID int } // тема чата уведомлений; 0 - без темы
func parseThreadID(raw string) (int, error) // "" - 0; иначе parsePositive("ALERT_THREAD_ID", raw)
```

`NewBot`: `b.alertChat = cfg.AlertChatID`, `b.alertThread = cfg.AlertThreadID`; лог
`alerts_enabled` после присваивания получает `thread_id` из `b.alertThread`, не из `cfg`.
`Notify`, ветка `p.ChatID != 0`: `opts := &tele.SendOptions{}`; при
`p.ChatID == b.alertChat` - `opts.ThreadID = b.alertThread`; `b.alert.Send(chat, p.Text, opts)`.

## 5a. Карта среза

Сверено `grep -n` в ветке `slice/alert-topic` (963f75d). Пути от `src/`, кроме `deploy/`.

| Файл | Строка | Символ | Что делает срез | Этап |
|---|---|---|---|---|
| `internal/app/config.go` | 38-39 | `Config.AlertBotToken`, `AlertChatID` | поле `AlertThreadID int` | 1 |
| `internal/app/config.go` | 125-129 | `LoadConfig`, разбор `ALERT_CHAT_ID` | разбор `ALERT_THREAD_ID` рядом | 1 |
| `internal/app/config.go` | 197 | `func parseChatID(raw string) (int64, error)` | образец для `parseThreadID` | 1 |
| `internal/app/config.go` | 264 | `func parsePositive(name, raw string) (int, error)` | переиспользовать | 1 |
| `internal/app/config_test.go` | 9 | `TestLoadConfigReportsEveryProblem` | образец окружения теста | 1 |
| `internal/app/bot.go` | 104 | `Bot.alert *tele.Bot` | поля `alertChat`, `alertThread` | 2 |
| `internal/app/bot.go` | 123, 177 | `func NewBot(ctx, cfg Config, ...) (*Bot, error)`, лог `alerts_enabled` | заполнить поля, `thread_id` в лог | 2 |
| `internal/app/bot.go` | 394, 411 | `func (b *Bot) Notify(ctx, job Job) error`, `b.alert.Send` | `SendOptions.ThreadID` | 2 |
| `internal/app/case.go` | 1062 | `type notifyPayload` | не меняется, для job в тесте | 2 |
| `internal/app/screen_harness_test.go` | 51, 161, 211 | `newFakeTelegram`, `methodCalls`, `screenBot` | обвязка тестов этапа 2 | 2 |
| `internal/app/screen_test.go` | 20 | `func notifyJob(t, caseID, text, buttons string) Job` | работа автору для рубежа | 2 |
| `internal/app/alert_test.go` | 13 | `const testAlertChat` | чат тестов | 2 |
| `deploy/prod/docker-compose.coolify.yml` | 51-52 | `ALERT_CHAT_ID`, `ALERT_BOT_TOKEN` | строка `ALERT_THREAD_ID: ${ALERT_THREAD_ID:-}` | 3 |
| `.env.example` | 16-23 | блок `ALERT_*` | `ALERT_THREAD_ID=` с комментарием | 3 |

## 6. Этапы реализации

Одна зона: `src/internal/app` плюс `deploy/prod`, `src/.env.example`.

| Этап | Результат | Тесты (первыми, красные) | Статус | Способ |
|---|---|---|---|---|
| 1. Конфиг | `ALERT_THREAD_ID` разобрана, кривое значение в списке проблем | `TestLoadConfigAlertThreadID` (таблица: `""`=0, `"5"`=5, `abc`/`0`/`-3` - ошибка с именем переменной) | pending | руками |
| 2. Отправка | тема в `sendMessage` уведомления | `TestNotifyAlertThread` (таблица по `ftg.methodCalls("sendMessage")`: `with_thread` - `message_thread_id="5"`; `no_thread` и `other_chat` - ключа нет); `TestNotifyAuthorIgnoresAlertThread` (БД, skip без `TEST_DATABASE_URL`) | pending | руками |
| 3. Конфиг контура | переменная в compose и `.env.example` | нет, `make commit-check` | pending | руками |

Проверка этапа: `make commit-check`; перед сдачей среза `make test` с `TEST_DATABASE_URL`.
`AGENTS.md:81` и `docs/contracts.md:39-43` (упомянуть `ALERT_THREAD_ID`) правит роль
документации, не worker.

## 7. Критерий приёмки

- Команда: `cd src && make ci-check`, зелёная.
- Сценарий: тесты этапов 1-2 и рубежи 3a зелёные.
- Не автоматизируется: доставка в тему 5 прода - раздел «Выкат», владелец и оркестратор.
- Не проверяем: удаление темы в живой группе (сценарий `400` покрыт политикой очереди).

## Выкат

Исполняют оркестратор и владелец, не worker. `coolify-deploy.sh` - из
`galera-dev-knowledge/infra/scripts/`, runbook `runbooks/coolify-service-api.md`. PATCH
несуществующей переменной даёт `404`: `ALERT_THREAD_ID` заводит Coolify из Compose.

1. Владелец: `ALERT_BOT_TOKEN` в `/etc/galera/secrets/galera/intake-dev.env` = Keychain
   `galera/platform/alert-bot`; `coolify-deploy.sh intake sync-env`; `set -a;
   . galera-infra/.env; set +a; coolify-deploy.sh intake set-env ALERT_CHAT_ID
   "$ALERT_CHAT_ID"` (агенту `.env` закрыт). Старый код шлёт в General группы, не теряет.
2. Релиз тегом `v*` с кодом среза, workflow `Deploy` по команде владельца: он
   синхронизирует Compose, переменная заводится пустой, старт с `thread_id=0`.
3. `coolify-deploy.sh intake list-env | grep ALERT_THREAD_ID` - переменная есть.
4. `coolify-deploy.sh intake set-env ALERT_THREAD_ID 5`, затем отдельный
   `coolify-deploy.sh intake deploy` (PATCH без деплоя не применяется).
5. `deploy/safe-ssh.sh app-logs 200`: `alerts_enabled on=true own_bot=true thread_id=5`.
6. Путь кода: владелец задаёт вопрос кнопкой «Спросить», уведомление о вопросе читается
   `tg_read` MCP `galera-mcp-tg` в теме 5. Нет его - тестовое `sendMessage` в тему по
   `plan-alert-channel.md:93-101` отделяет тему от кода; тестовое удаляется
   `deleteMessage` в окне 48 часов.

## 8. Обязательный хвост среза

| Шаг | Что именно | Отметка |
|---|---|---|
| Триаж тикета | #70 сверен разведкой `docs/briefing/70-recon.md` | в силе |
| Метки `status:` | на #70 по `workflow.md` | оркестратор |
| Регрессор | `regress` против инвариантов `AGENTS.md` | pending |
| Ревью и мердж | `code-reviewer`, мердж в `feature/alert-topic` оркестратором | pending |
| Полный прогон | `make ci-check` | pending |
| Журнал | `state.md`, `backlog.md`, `CHANGELOG.md` - `finish` и релиз | pending |
| Деплой | раздел «Выкат», `runbooks/coolify-service-api.md` | pending |
