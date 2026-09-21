# Спека среза: «Отправить как есть»

Статус: draft
Issue: galera-tasks#32, срез 5 из 8
Каноны: `docs/specs/ticket-form.md` (§2.1 правило 3, §3 «Повтор», §8 «пропуск вопросов» и
«разделы саммари», R5, Р-5, Р-15, §11), `plan-live-screen.md`, `docs/contracts.md`
Architecture review: pending

## 1. Цель и границы

- Цель: автор под любым раундом вопросов жмёт «Отправить как есть» и получает саммари с тем,
  что уже сказано; вопросов в этом обращении больше нет.
- Результат: команды §7 зелёные.
- Делаем: `Cases.SkipQuestions`, событие `questions_skipped`, кнопку в клавиатуре раунда,
  хендлер `onSkip`, отсев вопросов после пропуска (Р-15), канон.
- Не делаем: навигацию, панель, `OnCallback`-фолбэк (6); тексты кроме нужных кнопке (7);
  живой прогон R5 в песочнице (8).

## 2. Архитектура

| Блок | Изменение |
|---|---|
| `interview.go` | `SkipQuestions`, `skipped`; `roundAnswered` видит `questions_skipped`; `Run` и `saveTurn` не дают раунда после пропуска |
| `bot.go` | `skipBtn` (`Unique: "skip"`), `onSkip`; у `roundKeyboard` меняется сигнатура на `(round, suggested)`, `Notify` даёт клавиатуру и `keysAsk`; `onAllTrue` на `ErrRoundAnswered` - toast |

Писатель события - `Cases.SkipQuestions`, зовёт только `onSkip`. Статус не меняется, в
`summary` переводит `Summarize` (§8 глобальной). Миграции нет: `case_events.kind` - текст.

## 3. Сценарии

- Happy path (R5): раунд N на живом экране, «Отправить как есть» - событие, работа
  `summarize`, toast «Принято», экран раунда правится пометкой без кнопок; саммари приходит
  следующим шагом и снимает кнопки экрана по срезу 4 (no-op).
- Повтор: второе нажатие того же экрана - toast «Этот экран устарел», событие и работа одни.
- Нажатие после ответа (текст, голос, «Всё так») - то же.
- Ответ текстом после пропуска, до саммари: в очереди `summarize` и `interview`. Ход первым -
  без вопросов, `replaceJob` оставляет одну `summarize`, ответ в саммари. Саммари первым -
  ответ уже в нём, `Run` видит `summary` и ничего не делает. Оба порядка верны.
- Правка после саммари: ход без вопросов, саммари пересобрано, раунда нет (Р-15).

## 3a. Рубежи молчания

| Состояние | Чего не делаем | Чем держится | Тест |
|---|---|---|---|
| после `questions_skipped` в обращении | раунд вопросов автору не уходит: ни ходом после ответа, ни правкой саммари | `Run`: `skipped` - вопросы в ноль до `toSummary` | `TestAnswerAfterSkip`, `TestFixAfterSkip` |
| пропуск в другом обращении | вопросы этого не отбрасываются | `skipped` по `case_id` | `TestFixAfterSkip` |
| отказ пропуска: раунд отвечен, пропущен, не текущий, статус не `interview` | нет события, работы, сообщения; только toast | проверки `SkipQuestions` в транзакции до записи | `TestSkipRefused`, `TestSkipTwice`, `TestSkipAfterAnswer` |
| пропуск принят | публикации нет: сначала саммари (Р-5); статус и `round` прежние | событие и работа `summarize`; `UPDATE` пишет только `updated_at` | `TestSkipQuestions` |
| нажатие не на живом экране | нет вызова `SkipQuestions` | `liveScreen` до действия | `TestSkipRefused` |

## 3b. Сценарии проверки

Вынесены в [plan-skip-questions-3b.md](plan-skip-questions-3b.md).

## 4. Данные и состояния

`questions_skipped`, payload `{"round": N}`. Пропуск разрешён, когда одновременно:
`status = 'interview'`, `cases.round` равен номеру из кнопки, последнее из событий
`round_asked`, `answer_given`, `questions_skipped` - `round_asked`. После пропуска последнее
- `questions_skipped`, повтор упирается в то же условие.

| Событие против состояния | раунд без ответа | после ответа | после пропуска |
|---|---|---|---|
| «Отправить как есть» | пропуск | toast «Этот экран устарел» | toast «Этот экран устарел» |
| «Всё так» | прежний путь | toast «Этот экран устарел» | toast «Этот экран устарел» |
| текст, голос | ответ | ответ | ответ; ход без вопросов |

`history` событие не читает: в payload нет текста.

## 5. Кодовая модель

```go
var skipBtn = &tele.Btn{Unique: "skip"} // data - номер раунда, как у «Всё так»
func (c *Cases) SkipQuestions(ctx context.Context, cs *Case, round int) error
func (c *Cases) skipped(ctx context.Context, caseID string) (bool, error)
func roundKeyboard(round int, suggested bool) *tele.ReplyMarkup
func (b *Bot) onSkip(c tele.Context) error
```

- `SkipQuestions`: `cs.Status != interview` - `ErrNotInterview`; `round != cs.Round` -
  `ErrStaleRound`. В транзакции `UPDATE cases SET updated_at = now() WHERE id = $1 AND
  status = 'interview' AND round = $2` (0 строк - `ErrStaleRound`); последнее событие
  тройки §4: нет - `ErrStaleRound`, не `round_asked` - `ErrRoundAnswered`; `addEvent`,
  `replaceJob(JobSummarize)`, лог `questions_skipped`.
- `skipped`: `EXISTS` события `questions_skipped` обращения.
- `roundAnswered`: в `IN` добавляется `questions_skipped`, ответ - `kind != 'round_asked'`.
- `Run`: после `dropDetails`, если `skipped`, - `turn.Questions = nil`, лог `skip_dropped`
  с числом снятых (только если больше 0). Дальше прежний `toSummary`.
- `saveTurn`: после `UPDATE cases` (строка заблокирована) до ветки `round_asked` читает
  `skipped` через `tx`; true - ветка саммари. Закрывает пропуск во время хода модели.
- `onAllTrue`: `ErrRoundAnswered` - toast «Этот экран устарел» и `stripScreen` нажатого
  вместо «Ответ уже принят»: после пропуска оно обещало бы вопрос вопреки Р-15.
- `roundKeyboard`: одна строка; `suggested` - «Всё так» (`all_true|N`), затем «Отправить как
  есть» (`skip|N`); иначе только вторая. `Notify` для `keysRound` и `keysAsk`.
- `onSkip`: `Active` (ошибка - ответ на callback и возврат); `cs == nil` - как у «Всё так»;
  номер из data не число - toast «Этот экран устарел»; `liveScreen`; `SkipQuestions`:
  три отказа - toast «Этот экран устарел» и `stripScreen` нажатого; прочая ошибка - ответ
  на callback и возврат; успех - toast «Принято», `b.screen(c, markAnswered(c, "Отправляю
  как есть. Собираю саммари."), nil)`, `waitFor(cs)`.

## 6. Этапы реализации

1. Тесты 3b, красные. 2. `SkipQuestions`, `skipped`, `roundAnswered`, `Run`, `saveTurn`.
3. Кнопка, клавиатура, `onSkip`. 4. Канон: `prd.md` (кейс 1, строка развилки «автор нажал
«Отправить как есть»»), `architecture.md` (шаг 6 интервью, событие), `llm.md` (отсев после
пропуска рядом с `detail_dropped`), `contracts.md` (`Unique` `skip`).

## 7. Критерий приёмки

`L=postgres://intake:intake@localhost:5434/intake_w5?sslmode=disable`; URL - аргументами
make: `src/.env` перекрыл бы окружение.

```
m=0; make -C src ci-check DATABASE_URL=$L TEST_DATABASE_URL=$L || m=1
T='TestSkipQuestions TestSkipButton TestSkipTwice TestSkipAfterAnswer TestSkipRefused'
T="$T TestAnswerAfterSkip TestFixAfterSkip TestSkipDuringTurn TestRoundKeyboardSkip"
T="$T TestStepButtonAlwaysAnswers"
R="^($(echo $T | tr ' ' '|'))\$"
TEST_DATABASE_URL=$L go test -C src ./internal/app -v -count=1 -run "$R" > s5.log 2>&1
grep -q -- '--- SKIP' s5.log && echo skip && m=1
for t in $(echo $T); do grep -q -- "--- PASS: $t (" s5.log || { echo "нет $t"; m=1; }; done
[ $m = 0 ] && echo ok
```

- Последняя строка `ok`: `ci-check` зелёный, каждый тест 3b прошёл, ни один не пропущен.
- Не автоматизируется: вид кнопок в клиенте, диалог R5 через `tg_send`/`tg_read` - срез 8.
- Не проверяем: сбой Telegram на правке экрана после пропуска (путь `b.screen`, прежний).

## 8. Обязательный хвост среза

| Шаг | Что именно | Отметка |
|---|---|---|
| Триаж | #32 в работе | in-progress |
| Регрессор | `regress` по §3a: раунд не уходит после пропуска, отказ молчит | |
| Ревью и PR | `code-reviewer`, коммит в `feature/ticket-form` | |
| Полный прогон | команды §7 | |
| Журнал | `docs/state.md`, строка среза в backlog | |
| Деплой | уходит релизом функции, миграции нет | |

## 9. Решения среза

1. Кнопка под каждым раундом, включая открытый правкой: R5 не называет номер раунда.
2. Номер раунда в `callback_data`, как у «Всё так»: при `screen_msg = 0` (обращения до
   `0012`) это единственная защита от кнопки прошлого раунда.
3. Любой отказ пропуска и «Всё так» на отвеченном раунде - toast «Этот экран устарел».
4. Пропуск - событие журнала, не колонка: миграции нет, признак живёт весь срок обращения.
5. Повтор и «после ответа» - одно правило: последнее событие тройки §4.
6. Ответ после пропуска принимается («ни одна идея не выбрасывается»). `markRound` среза 4
   перепишет «Отправляю как есть» на «Ответ принят» - это правда, условия не вводим.
7. Блокировка строки через `UPDATE ... updated_at`: пропуск - действие автора, таймер
   черновика сдвигается, как от ответа.
8. Тексты «Принято», «Отправляю как есть. Собираю саммари.» временные, правит срез 7.
