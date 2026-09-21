# Сценарии проверки среза «Отправить как есть»

Раздел 3b [plan-skip-questions.md](plan-skip-questions.md). Фейковый Telegram и сборка `Bot` -
харнесс среза 4 (`screen_harness_test.go`): вызовы пишутся с методом, `message_id`,
`reply_markup`, текстом toast. Модель - стаб OpenRouter по образцу `interview_test.go`.
Все строки - ярус БД. Обращение по умолчанию: `bug`, `interview`, `contract` без `case`,
`gaps = [case]`, `round_asked` 1 с вопросом по `case` с догадкой, `cases.round = 1`,
`screen_msg = 100`, `screen_round = 1`.

| # | Дано / когда / тогда | Факт | Тест |
|---|---|---|---|
| 1 | R5: `SkipQuestions(cs, 1)`; затем работа из очереди выполнена, стаб саммари без раздела `case` | событие `questions_skipped` `{"round":1}`; работа `summarize` одна, `interview` и `publish` нет; до работы статус `interview`, `round = 1`; после - `summary`, notify `keysSummary` с «Не уточнено: конкретный случай.» | `TestSkipQuestions` |
| 2 | нажатие `skip|1` с id 100 | toast «Принято»; `editMessageText(100)` без `reply_markup`, текст раунда + «Отправляю как есть»; событие есть | `TestSkipButton` |
| 3 | R5 повтор: `skip|1` с id 100 дважды | второе: toast «Этот экран устарел», снятие 100; событие одно, работа `summarize` одна | `TestSkipTwice` |
| 4 | «Всё так» (`all_true|1`) с id 100 после пропуска | toast «Этот экран устарел», снятие 100; `answer_given` нет | `TestSkipTwice` |
| 5 | R5 после ответа: `AddAnswer` текстом, затем `skip|1` с id 100 | toast «Этот экран устарел»; `questions_skipped` нет; работа `interview` на месте, `summarize` нет | `TestSkipAfterAnswer` |
| 6 | то же после «Всё так» | то же | `TestSkipAfterAnswer` |
| 7 | не живой экран: `skip|1` с id 99 | toast «Этот экран устарел», снятие 99; события и работы нет | `TestSkipRefused` |
| 8 | `screen_msg = 0`, `cases.round = 2`, `skip|1` | toast «Этот экран устарел»; события нет | `TestSkipRefused` |
| 9 | `screen_msg = 0`, статус `summary`, `skip|1` | то же, статус `summary` | `TestSkipRefused` |
| 10 | data `skip|x` | toast «Этот экран устарел»; события нет | `TestSkipRefused` |
| 11 | Р-15: пропуск, затем `AddAnswer` до саммари; `Run` по работе `interview` раньше оставшейся `summarize`; стаб хода вернул вопрос по `case`, `ready = false` | `round_asked` не добавлен, `interview_done`, notify раунда нет; работа `summarize` в `jobs` одна; запрос саммари к стабу содержит текст ответа | `TestAnswerAfterSkip` |
| 12 | R5 правка: пропуск, саммари показано, `AddAnswer` правкой; стаб хода вернул вопрос | `round_asked` не добавлен, `interview_done`, работа `summarize`; `cases.round` прежний | `TestFixAfterSkip` |
| 13 | то же у второго обращения без пропуска | `round_asked` 2 записан: отсев не течёт между обращениями | `TestFixAfterSkip` |
| 13a | повтор работы `interview` после закоммиченного `round_asked` 1; стаб модели в обработчике зовёт `SkipQuestions(cs, 1)` и отвечает вопросом по `case` | `round_asked` 2 нет, `interview_done`, работа `summarize`: держит проверка в `saveTurn` | `TestSkipDuringTurn` |
| 14 | `Notify` `keysRound` раунда 2; `keysAsk` раунда 2 | `sendMessage`: одна строка `all_true|2`, `skip|2`; для `keysAsk` только `skip|2`; тексты «Всё так», «Отправить как есть»; `callback_data` не длиннее 64 байт | `TestRoundKeyboardSkip` |
| 15 | `onSkip` в таблице среза 4: ошибка `Active`, `cs == nil` | `answerCallbackQuery` на нажатие | `TestStepButtonAlwaysAnswers` |

Не тестируется: отказ Telegram на правке экрана после пропуска - путь `b.screen`, прежний.
