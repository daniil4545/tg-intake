# Спека среза: живой экран обращения

Статус: agreed
Issue: galera-tasks#32, срез 4 из 8
Каноны: `docs/specs/ticket-form.md` (§2.1 правила 1-3, 5; §8 «живой экран»; R8; Р-8), `docs/contracts.md`
Architecture review: pass with fixes, находки M1, M2, S1-S7 закрыты текстом

## 1. Цель и границы

- Цель: живой экран обращения в `cases.screen_msg`; новый шаг снимает кнопки прошлого;
  кнопка шага не на живом экране - toast «Этот экран устарел».
- Результат: команды §7 зелёные; после рестарта пометка раунда и «Всё так» правят экран из БД.
- Делаем: `0012`, `Case.Screen`, `SetScreen`, шаги и переходы через экран, правило 3 для
  «Всё так», «Публикую», «Поправить»; карты `tally`, `rounds` уходят; канон.
- Не делаем: «Отправить как есть» (5); навигацию, правило 2, панель, исход отмены тикета и
  `kills` (6); тексты кроме toast правила 3 (7); ответ по документации - не шаг.

## 2. Архитектура

| Блок | Изменение |
|---|---|
| `0012_live_screen.sql` | `cases.screen_msg bigint`, `cases.screen_round int`, оба `NOT NULL DEFAULT 0`; down - drop |
| `case.go` | `Case.Screen`, `Case.ScreenRound` в `caseColumns`, `SetScreen`, `ResetScreen` |
| `interview.go` | `RoundView`: вопросы последнего `round_asked` и число ответов после него |
| `bot.go` | `showStep`, `closeScreen`, `stripScreen`, `liveScreen`; `Notify`, `countItem`, `markRound`, хендлеры |

Писатель экрана - `Bot` через `SetScreen`, `ResetScreen`: поллер в сборе и на переходах,
воркер (`Notify`) в `interview`/`summary`. Гонка допустима, лишнюю копию ловит правило 3.

## 3. Сценарии

- Повтор, рестарт - строки 3-6, 10-11 §3b. `SetScreen` не записан после отправки - ошибка
  `Notify`, повтор шлёт шаг снова (§8), первую копию ловит правило 3.
- Второе «Публикую» на живом экране - прежний ответ «Уже публикую» (`ErrNoSummary` при
  `publishing`), работа одна по ключу.
- Telegram не снял кнопки - лог `screen_strip_failed`, шаг уходит.

## 3a. Рубежи молчания

| Состояние | Чего не делаем | Чем держится | Тест |
|---|---|---|---|
| новость (`keysTicket`), уведомление владельцу, сообщение без кнопок (протокол, напоминание) | не снимаем кнопки, не пишем экран | ветки `Notify` до `showStep` | `TestNewsKeepsScreen` |
| `screen_msg = 0` | нет вызова Telegram с id 0, нет проверки правила 3 | `stripScreen`, `liveScreen` | `TestStepWithoutScreen` |
| нажатие не на живом экране | нет `answer_given`, `ConfirmSummary`, работ | `liveScreen` до действия | `TestStaleStepButton` |
| экран не раунд ответа (саммари, раунд до недоставленного) | пометка не садится на чужой экран | `markRound`: `interview` и `ScreenRound == cs.Round` | `TestFixTextKeepsSummary`, `TestMarkSkipsOtherRound` |
| переход не прошёл в БД | кнопки живого экрана не снимаем | `closeScreen` после успеха | `TestFailedTransitionKeepsScreen` |
| любое | `updated_at` не трогаем: на нём `SweepDrafts`, `RemindDrafts` | запросы экрана пишут только его колонки | `TestScreenSurvivesRestart` |

## 3b. Сценарии проверки

Вынесены в [plan-live-screen-3b.md](plan-live-screen-3b.md), 20 строк.

## 4. Данные и состояния

`screen_msg`: 0 - экрана нет; чат равен `user_id`. `screen_round`: номер раунда на экране,
0 - экран не раунд (счётчик, саммари). Шаг - снять старый, отправить, записать новый;
переход - снять, записать 0; правка на месте экран не меняет.

| Место | Класс | Экран |
|---|---|---|
| `countItem` | шаг | `Screen == 0` или отказ правки - `showStep`; иначе правка `Screen` |
| `Notify` `keysRound`, `keysAsk` | шаг | `showStep`, раунд `cs.Round` |
| `Notify` `keysSummary` | шаг | `showStep`, раунд 0 |
| `Notify` `keysHome` | переход | `closeScreen`, затем отправка |
| `Notify` `keysAnswer` | вне шагов | `ResetScreen` без снятия, вместо `dropTally` |
| `onDone` после `FinishCollect == nil`; `onContinue` в ветке `collecting`; `onReset`, `onResetYes` после `CancelCase == nil`; `onToTicket` при `switched` | переход | `closeScreen` |

## 5. Кодовая модель

```go
Screen, ScreenRound int // в Case: message_id живого экрана и его раунд, 0 - нет
func (c *Cases) SetScreen(ctx context.Context, caseID string, msgID, round int) error
func (c *Cases) ResetScreen(ctx context.Context, caseID string, msgID int) error
func (c *Cases) RoundView(ctx context.Context, caseID string) (questions []Question, answers int, err error)
func (b *Bot) showStep(ctx context.Context, cs *Case, round int, text string, opts ...any) error
func (b *Bot) closeScreen(ctx context.Context, cs *Case)
func (b *Bot) stripScreen(cs *Case, msgID int)
func (b *Bot) liveScreen(c tele.Context, cs *Case) bool
```

- `SetScreen`: `UPDATE cases SET screen_msg = $2, screen_round = $3 WHERE id = $1`.
- `ResetScreen`: `SET screen_msg = 0, screen_round = 0 WHERE id = $1 AND screen_msg = $2`:
  экран, записанный параллельным шагом, не затирается.
- `stripScreen`: id 0 - ничего; `EditReplyMarkup(msg, nil)`; `ErrMessageNotModified`,
  `ErrSameMessageContent` - успех; прочее - `Warn screen_strip_failed`, наверх не идёт.
- `showStep`: снять `cs.Screen`, `sendLong` (кнопки на последнем куске - он экран),
  `SetScreen(sent.ID, round)`; ошибки отправки и записи - наверх.
- `closeScreen`: снять, `ResetScreen(cs.Screen)`; ошибка записи - `Warn screen_reset_failed`.
- `liveScreen`: `Screen == 0` или id нажатого равен `Screen` - true; иначе toast, снятие
  нажатого, false. Зовут `onAllTrue`, `onPublish`, `onFix` после `Active` и до своего toast.
- `markRound`: правит `Screen`, только если `statusInterview`, `Screen != 0`,
  `ScreenRound == cs.Round` (раунд ответа) и текст `roundMessage` из `RoundView` с пометкой
  по `answers` не длиннее `maxMessage`; иначе false, ответ новым сообщением.
- `RoundView`: `lastQuestions` и `count(*)` `answer_given` с `id` больше последнего `round_asked`.

## 6. Этапы реализации

1. Тесты §3b, красные. 2. `0012`, поля `Case`, `SetScreen`, `ResetScreen`, `RoundView`.
3. Помощники, `Notify`, `countItem`, `markRound`; `tally`, `rounds` удаляются. 4. `liveScreen`,
переходы. 5. Канон: `contracts.md` (живой экран, правило 3), `architecture.md` (колонки,
память `Bot`).

## 7. Критерий приёмки

- `make -C src ci-check DATABASE_URL=<лок> TEST_DATABASE_URL=<лок>` - аргументами make:
  `src/.env` перекрыл бы окружение.
- `TEST_DATABASE_URL=<лок> go test -C src ./internal/app -v -count=1 -run '<имена §3b>'
  > s4.log; ! grep -q -- '--- SKIP' s4.log && for t in <имена §3b>; do grep -q -- "--- PASS: $t" s4.log || echo "нет $t"; done`
  - пусто в выводе и код 0: каждый тест §3b прошёл, ни один не пропущен.
- Не автоматизируется: вид экранов в клиенте - срез 8. Не проверяем: снятие старше 48 часов (§9).

## 8. Обязательный хвост среза

| Шаг | Что именно | Отметка |
|---|---|---|
| Триаж | #32 в работе | in-progress |
| Регрессор | `regress` по §3a | |
| Ревью и PR | `code-reviewer`, коммит в `feature/ticket-form` | |
| Полный прогон | команды §7 | |
| Журнал | `docs/state.md`, строка среза в backlog | |
| Деплой | `0012` уходит релизом функции по runbook выката | |

## 9. Внешний факт и решения среза

Bot API, `editMessageReplyMarkup` (проверено 21.09): «Note that business messages that were
not sent by the bot and do not contain an inline keyboard can only be edited within 48 hours
from the time they were sent». Предел назван только для бизнес-сообщений не от бота; прямого
«без предела» для своих сообщений нет. Поведение безопасно при обоих ответах: отказ снятия -
лог, устаревшую кнопку ловит правило 3; вопрос закрывает `screen_strip_failed` на контуре.

1. Исход отмены тикета и `kills` - срез 6: навигация §2.1 (§8, §13 глобальной спеки поправлены).
2. Счётчик сбора - живой экран без кнопок, заменяет `tally` (Р-8); снятие у него - no-op.
3. Ответ по документации не шаг: кнопки остаются, экран обнуляется, как `dropTally`.
4. Toast правила 3 только у кнопок шага; фолбэк `OnCallback` и «Это кнопка от прошлого
   вопроса» (путь `screen_msg = 0`) - срезы 6, 7.
5. Хендлер шага читает обращение до toast: callback отвечается один раз, текст toast зависит
   от проверки. Цена - спиннер на один `SELECT`. Каждый выход хендлера кнопки шага до
   `liveScreen` (ошибка `Active`, `cs == nil`) отвечает на callback (S3).
6. Отказ `ResetScreen` на переходе - лог: переход автор уже получил.
7. `screen_round` (M1): без него пометка ответа садилась на саммари или на экран прошлого
   раунда, когда следующий записан, а `Notify` ещё не доставлен.
8. `bigint`: `message_id` в Bot API - Integer без обещания 32 бит.
9. После «Всё так» следующий текст автора перепишет «Принято: всё так» в «Принято ответов:
   N» (раньше `dropRound` отдавал его новым сообщением): `AcceptRound` тоже пишет
   `answer_given`. Принято (S4): экран раунда один, счёт честный.
