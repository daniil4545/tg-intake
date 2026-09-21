# Спека среза: навигация и нижняя панель

Статус: agreed
Issue: galera-tasks#32, срез 6 из 8
Каноны: `docs/specs/ticket-form.md` (§2.1 правила 1-4; §8 «живой экран», «нижняя панель»; R8; Р-8), `docs/contracts.md`, [plan-live-screen.md](plan-live-screen.md), [plan-skip-questions.md](plan-skip-questions.md)
Architecture review: pass with fixes, находки M1, M2, S1-S4, S6 закрыты текстом

## 1. Цель и границы

- Цель: навигация по §2.1 вне обращения; исход отмены тикета - на карточку по `screen_msg`;
  панель - только переходом; устаревшая кнопка - один toast «Этот экран устарел».
- Результат: команды §7 зелёные; домашний экран - одно сообщение.
- Делаем: общий путь правки (правило 2, длина), домашний экран, переходы с панелью, отмену
  тикета через `screen_msg`, `staleButton` на всех отказах устаревшей кнопки, канон.
- Не делаем: «Отправить как есть» (5); тексты кроме toast §11 (7).

## 2. Архитектура

Поверх кода срезов 4 и 5. Миграций нет.

| Место | Изменение |
|---|---|
| `Bot.kills`, `killScreen`, `setKill`/`getKill`/`dropKill` | удаляются (Р-8) |
| `NewBot`: регистрация хендлеров | вынесена в `routes(tb)`; фолбэк `tele.OnCallback` - `staleButton` |
| `Notify`: ветка `keysCancel` | `b.finishKill(ctx, cs, p.Text)` |
| `finishKill` | экран из `cs.Screen`; отказ правки - `SetScreen` на новое (M2, как `onFix`) |
| `tickets.go`: `Tickets.Cancel` | параметр `msgID`, пишет `screen_msg` своей транзакцией |
| `onKill` | `setKill` уходит, `msg.ID` - в `Cancel` |
| `screen` | обёртка над `editScreen`, сигнатура прежняя |
| `stripScreen` | тело - вызов `stripButtons`, сигнатура и лог прежние |
| `liveScreen`; `onSkip` и ветка `ErrRoundAnswered` в `onAllTrue` (срез 5) | toast и снятие - `staleButton` |
| `onAllTrue`, `onResetYes`, `onCard`, `onKill`, `cardTarget` | порядок: разбор, отказ - `staleButton`, затем toast действия |
| `onFix` | запасной путь правки записывает экран |
| `showTickets`, `onCard` | ветка «длиннее `maxMessage` - `sendLong`» уходит в `editScreen` |
| `homeScreen` | одно сообщение |
| `onReset`, `onResetYes`, `onEndAsk` | хвост `homeScreen(intro)` - `sendPanel(intro)` и `homeScreen("")` |
| `onDone`: ветка `cs == nil` | ответ несёт `homeKeyboard()` |
| новые | `editScreen`, `stripButtons`, `staleButton`, `sendPanel`, `parseCard` |

## 3. Сценарии

- Отмена тикета: «Отменить тикет» на карточке 300 - `Cancel` пишет `screen_msg = 300` вместе
  с работой, карточка правится в «Отменяю тикет #N...»; notify `keysCancel` правит 300
  исходом с «К списку» (`slug:0`). Рестарт между ними ничего не меняет; повтор notify - та же правка.
- Устаревшая кнопка: один toast «Этот экран устарел», снятие кнопок, больше ничего.
- Отказ правки - правило 2; отказ отправки - ошибка наверх; отказ снятия - лог.

## 3a. Рубежи молчания

| Состояние | Чего не делаем | Чем держится | Тест |
|---|---|---|---|
| активное обращение с живым экраном, автор ходит по навигации | не снимаем кнопки и не пишем `screen_msg` | навигация не зовёт `SetScreen`, `closeScreen` | `TestNavigationKeepsLiveScreen` |
| исход отмены тикета A при активном обращении B | экран и панель B не трогаем | `finishKill` правит только `A.Screen`, без reply-клавиатуры | `TestKillOutcomeKeepsActiveCase` |
| отмена не прошла (`ErrNotAuthor`, `ErrIssueGone`) | `screen_msg` не пишем | запись в транзакции `Cancel` после проверок | `TestKillScreenOnlyOnCancel` |
| устаревшая «Всё так» (битые data, `ErrStaleRound`, `ErrNotInterview`) | нет `answer_given`, нет работы | `staleButton` до и вместо `AcceptRound` | `TestStaleButtonRefused` |
| домашний экран без перехода | панель не шлём | `homeScreen` без reply-клавиатуры | `TestHomeScreenOneMessage` |
| незнакомая кнопка | нет сообщения, нет записи в БД | `staleButton` | `TestUnknownButtonStale` |

## 3b. Сценарии проверки

В [plan-navigation-3b.md](plan-navigation-3b.md), там же карта входов и `finishKill`.

## 4. Данные и состояния

`screen_msg` опубликованного обращения - карточка «Отменить тикет»: пишет `Cancel`, читает
`finishKill`, не обнуляется (правило 3 неактивное не читает). Панель не хранится; карта
входов - в 3b.
- `Notify` при `collecting` ставит «Готово | Сброс»: возврат в сбор - снова начало сбора.
- Режим вопроса: `onDone` оставляет «Готово | Сброс» - разговор продолжается в сборе.

## 5. Кодовая модель

```go
func (b *Bot) editScreen(to tele.Recipient, msg tele.Editable, text string, markup *tele.ReplyMarkup) (*tele.Message, error)
func (b *Bot) stripButtons(msg tele.Editable) error
func (b *Bot) staleButton(c tele.Context) error
func (b *Bot) sendPanel(c tele.Context, text string) error
func parseCard(data string) (slug string, number int, ok bool)
func (t *Tickets) Cancel(ctx context.Context, project Project, number int, userID int64, msgID int) (string, error)
```

- `editScreen`: текст длиннее `maxMessage` или `msg == nil` - `sendLong` и снятие `msg`;
  иначе `Edit`; `ErrSameMessageContent`, `ErrMessageNotModified` - успех; прочий отказ -
  `Warn screen_edit_failed`, `sendLong`, снятие `msg`. Возврат: nil при правке, новое
  сообщение при запасном пути; ошибка отправки - наверх, отказ снятия - только лог.
- `screen(c, ...)` = `editScreen(c.Recipient(), c.Message(), ...)`, сообщение отбрасывается.
  `onFix` зовёт `editScreen` сам: не-nil - `SetScreen(cs.ID, sent.ID, 0)`.
- `stripButtons`: `EditReplyMarkup(msg, nil)`; «not modified» - nil.
- `staleButton`: `toast("Этот экран устарел")`; `c.Message() != nil` - `stripButtons`, отказ -
  `Warn screen_strip_failed`; возвращает nil. Единственный ответ на callback в этом нажатии.
- `onAllTrue`: `Active` - без toast, наверх, отвечает `OnError` (второй `Respond` Telegram
  отклонил бы, автору не видно); `cs == nil` - `toast("")`, `screen`; `liveScreen`; `Atoi` -
  ошибка `staleButton`; `AcceptRound`: `ErrStaleRound`, `ErrNotInterview`, `ErrRoundAnswered` -
  `staleButton`; `ErrNoSuggestion` - прежний ответ; прочая ошибка - тем же путём; успех -
  `toast("Принято")`, прежнее.
- `onCard`, `onKill`: `parseCard(c.Data())` - не ok: `staleButton`, возврат; затем toast
  действия; `cardTarget` оставляет поиск проекта.
- `onResetYes`: `Active` - тем же путём; `cs == nil` - `toast("")`, `screen`; `data != cs.ID` -
  `staleButton`, возврат; затем toast «Сбрасываю».
- `sendPanel`: `c.Send(text, homeKeyboard())`. `homeScreen`: `homeText`, при непустом
  `intro` - `intro + "\n\n" + homeText`; `projectsMarkup`; один `c.Send`.
- `Cancel`: в транзакции после проверки автора - `UPDATE cases SET screen_msg = $2`
  (`msgID == 0` - без записи), затем `replaceJob`.

## 6. Этапы реализации

1. Тесты §3b, красные; счёт `answerCallbackQuery` в фейке. 2. `editScreen`, `stripButtons`,
`staleButton`, `screen`, `stripScreen`, `liveScreen`, `onSkip`, `onFix`, гвардии списка и
карточки. 3. Порядок toast в `onAllTrue`, `onResetYes`, `onCard`, `onKill`, `parseCard`.
4. `homeScreen`, `sendPanel`, переходы. 5. `Cancel`, `onKill`, `finishKill`, удаление `kills`.
6. `contracts.md`: правила 2, 4, staleButton.

## 7. Критерий приёмки

`<лок>` = `postgres://intake:intake@localhost:5434/intake_w6?sslmode=disable`, базы -
аргументами make: `src/.env` перекрыл бы окружение.

- `make -C src ci-check DATABASE_URL=<лок> TEST_DATABASE_URL=<лок>`
- `T='<имена из конца §3b>'`; `TEST_DATABASE_URL=<лок> go test -C src ./internal/app -v -count=1
  -run "^($(echo $T | tr ' ' '|'))$" > s6.log && ! grep -q -- '--- SKIP' s6.log &&
  (for t in $(echo $T); do grep -q -- "--- PASS: $t (" s6.log || exit 1; done)` - код 0
  только если каждый тест прошёл и ни один не пропущен.
- Не автоматизируется: вид экранов в клиенте - срез 8.

## 8. Обязательный хвост среза

| Шаг | Что именно | Отметка |
|---|---|---|
| Триаж | #32 в работе | in-progress |
| Регрессор | `regress` по §3a | |
| Ревью и PR | `code-reviewer`, коммит в `feature/ticket-form` | |
| Полный прогон | команды §7 | |
| Журнал | `docs/state.md`, строка среза в backlog | |
| Деплой | релизом функции, миграций у среза нет | |

## 9. Решения среза

1. Домашний экран без перехода приходит без панели; у нового автора её нет до первого сбора
   (правило 4). Переход с домашним экраном - два сообщения: панель и inline в одно не
   помещаются (§9 глобальной).
2. «Готово» и «Сброс» без обращения - переход: панель сбора остаётся после `SweepDrafts`,
   который отменяет черновик молча.
3. `sendState` в сборе и «Готово» в режиме вопроса оставляют «Готово | Сброс»: смены нет.
4. Остаточный риск S1: исход отмены может прийти раньше, чем `onKill` поправит карточку в
   «Отменяю...», и правка его затрёт. Окно - одна правка Telegram против работы с GitHub;
   цена - «Отменяю...» вместо исхода, исход виден в карточке.
5. Две карточки одного тикета, отмена с обеих: исход на нажатую последней, работа одна.
6. Исход отмены - на первую страницу списка (Р-8); «Отменяю...» хранит страницу нажатия.
7. Устаревшая кнопка отвечает только toast: toast действия ставится после разбора, иначе
   второй ответ на callback Telegram отклоняет.
