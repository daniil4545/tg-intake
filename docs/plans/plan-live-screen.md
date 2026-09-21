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

Вынесены в [plan-live-screen-3b.md](plan-live-screen-3b.md), 23 строки (20 плана + 3 ревью).

## 4. Данные и состояния

`screen_msg`/`screen_round`: 0 - нет экрана / не раунд ответа (счётчик, саммари), чат равен
`user_id`. Шаг - снять старый, отправить, записать новый; переход - снять, обнулить; правка на
месте экран не меняет.

| Место | Класс | Экран |
|---|---|---|
| `countItem` | шаг | `Screen == 0` или отказ правки - `showStep`; иначе правка `Screen` |
| `Notify` `keysRound`, `keysAsk` | шаг | `showStep`, раунд `cs.Round` |
| `Notify` `keysSummary` | шаг | `showStep`, раунд 0 |
| `Notify` `keysHome` | переход | `closeScreen`, затем отправка |
| `Notify` `keysAnswer` | вне шагов | `ResetScreen` без снятия, вместо `dropTally` |
| `onDone`/`onEndAsk`/`onToTicket` после успешного перехода; `onContinue` в `collecting`, после проверки проекта; `onReset`/`onResetYes` после `CancelCase == nil` | переход | `closeScreen` |

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
func (b *Bot) markRound(ctx context.Context, cs *Case, first string) bool
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
  нажатого, false. Зовут `onAllTrue`/`onPublish`/`onFix` после `Active`, до своего toast.
- `markRound(ctx, cs, first)`: правит `Screen`, только если `statusInterview`, `Screen != 0`,
  `ScreenRound == cs.Round` и текст `roundMessage` из `RoundView` с пометкой (`first` при одном
  ответе, «Принято ответов: N» при нескольких) не длиннее `maxMessage`; иначе false, ответ
  новым сообщением, нажатую кнопку (если была) снимает вызывающий.
- `RoundView`: `lastQuestions` и `count(*)` `answer_given` с `id` больше последнего `round_asked`.

## 6. Этапы реализации

1. Тесты §3b, красные. 2. `0012`, поля `Case`, `SetScreen`, `ResetScreen`, `RoundView`.
3. Помощники, `Notify`, `countItem`, `markRound`; `tally`, `rounds` удаляются. 4. `liveScreen`,
переходы. 5. Канон: `contracts.md` (живой экран, правило 3), `architecture.md` (колонки).

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
from the time they were sent». Предел - только у бизнес-сообщений не от бота, для своих явного
«без предела» нет: отказ снятия - лог, кнопку ловит правило 3.

1. Исход отмены тикета и `kills` - срез 6: навигация §2.1 (§8, §13 глобальной спеки поправлены).
2. Счётчик сбора - живой экран без кнопок, заменяет `tally` (Р-8); снятие у него - no-op.
3. Ответ по документации не шаг: кнопки остаются, экран обнуляется, как `dropTally`.
4. Toast правила 3 только у кнопок шага; фолбэк `OnCallback` и «Это кнопка от прошлого
   вопроса» (путь `screen_msg = 0`) - срезы 6, 7.
5. Хендлер шага читает обращение до toast, текст toast зависит от проверки. `cs == nil` -
   ожидаемый исход, отвечает сам; отказ `Active` не отвечает - ответит `OnError`, второй
   `Respond` был бы дублем (ревью).
6. Отказ `ResetScreen` - только лог, без return: на переходе автор уже получил ответ, а после
   `keysAnswer` повтор работы прислал бы тот же ответ вторым сообщением (ревью).
7. `screen_round` (M1): без него пометка ответа садилась на саммари или на экран прошлого
   раунда, когда следующий уже записан, а `Notify` не доставлен. Тот же риск для самого
   `Notify` (ревью): `keysRound`/`keysAsk` несут раунд в payload (`putNotifyRound`), шаг только
   при `interview && payload.Round >= cs.Round`; `keysSummary` - только при `summary`; `keysHome`
   закрывает экран, только если `ScreenRound == 0`. Иначе - сообщение без кнопки, экран цел.
8. `bigint`: `message_id` в Bot API - Integer без обещания 32 бит.
9. «Всё так» переиспользует `markRound` (S4): первый ответ - «Принято: всё так», следующие -
   общий счётчик «Принято ответов: N», `AcceptRound` тоже пишет `answer_given`.
