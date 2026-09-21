# Сценарии проверки среза «живой экран»

Раздел 3b [plan-live-screen.md](plan-live-screen.md). Фейковый Telegram - `httptest` по
образцу `poller_test.go`, пишет вызовы (метод, `message_id`, `reply_markup`, текст toast);
`Bot` собирается без `NewBot`, контекст - `tb.NewContext`. Все строки - ярус БД.

| # | Дано / когда / тогда | Факт | Тест |
|---|---|---|---|
| 1 | строка при `UpTo(11)`; `UpTo(12)`; down | `screen_msg = 0`, `screen_round = 0`; после down колонок нет | `TestMigration0012` |
| 2 | `SetScreen(id, 777, 2)` одним `Cases`, `Load` и `Active` новым | `Screen == 777`, `ScreenRound == 2`, `updated_at` прежний | `TestScreenSurvivesRestart` |
| 3 | `screen_msg = 100`; «Публикую», «Всё так», «Поправить» с id 99 | toast «Этот экран устарел», снятие 99, работ и `answer_given` нет, статус прежний | `TestStaleStepButton` |
| 4 | то же с id 100 | работа `publish`, toast «Публикую» | `TestStaleStepButton` |
| 5 | `screen_msg = 0`, «Публикую» с 99 | работа `publish` | `TestStepWithoutScreen` |
| 6 | S2: «Публикую» с 100 дважды | второе - «Уже публикую», работа `publish` одна, toast устаревшего экрана нет | `TestPublishTwice` |
| 7 | R8: `Notify` `keysRound` при `screen_msg = 100` | снятие 100 раньше `sendMessage`; `screen_msg` = новый id, `screen_round = cs.Round` | `TestStepStripsPrevious` |
| 8 | снятие отвечает 400 `message can't be edited` | шаг отправлен, `screen_msg` новый, лог `screen_strip_failed` | `TestStripFailureKeepsStep` |
| 9 | снятие отвечает `message is not modified` | лога нет | `TestStripFailureKeepsStep` |
| 10 | R8: `round_asked` 1, экран 100 раунда 1, новый `Bot`; два текста автора | `editMessageText(100)`: раунд + «Ответ принят», затем «Принято ответов: 2» | `TestRoundMarkAfterRestart` |
| 11 | R8: новый `Bot`, «Всё так» с 100 | `answer_given`, правка 100 | `TestRoundMarkAfterRestart` |
| 12 | M1: `summary`, экран саммари 100; «Поправить», два текста правки | `editMessageText(100)` только от «Поправить»; тексты - ответ новым сообщением | `TestFixTextKeepsSummary` |
| 13 | M1: экран раунда 1, `round_asked` 2 записан, `Notify` не доставлен; текст автора | правки 100 нет, ответ новым сообщением | `TestMarkSkipsOtherRound` |
| 14 | S7: текст раунда с пометкой длиннее `maxMessage` | правки нет, ответ новым сообщением | `TestMarkSkipsOtherRound` |
| 15 | сбор: до первого материала `screen_msg = 0`; два материала, новый `Bot`, третий | `stripScreen` не дёргает Bot API с `message_id 0`; один `sendMessage`, дальше правка того же id | `TestTallyIsScreen` |
| 16 | `Notify` `keysHome`; «Готово» после `FinishCollect`; «Да, сбросить» | снятие экрана, `screen_msg = 0` | `TestTransitionClosesScreen` |
| 17 | M2: «Готово» без материала (`ErrNoItems`); «Продолжить» в `summary`; «Да, сбросить» при `publishing` | снятия нет, `screen_msg` прежний | `TestFailedTransitionKeepsScreen` |
| 18 | S1: `screen_msg` сменился на 200 между чтением и `closeScreen(100)` | `screen_msg = 200` | `TestFailedTransitionKeepsScreen` |
| 19 | `Notify` `keysTicket` и без кнопок при `screen_msg = 100` | снятия нет, `screen_msg = 100` | `TestNewsKeepsScreen` |
| 20 | S3: «Публикую», «Всё так», «Поправить» без активного обращения - отвечает сам; `Active` вернул ошибку - не отвечает, возвращает ошибку (ответит `OnError`, ревью) | `answerCallbackQuery` разово / отказ без ответа | `TestStepButtonAlwaysAnswers` |
| 21 | ревью: `Notify` `keysRound` раунда 1 доставлен, когда живой экран уже раунда 2; то же после перехода в `summary` | живой экран не снят и не тронут, сообщение без кнопки шага | `TestStaleRoundNotify` |
| 22 | ревью, S4: «Всё так» приняло ответ (`AcceptRound`), но `markRound` не поправил экран (чужой `screen_round`) | нажатая кнопка снята, ответ ушёл новым сообщением | `TestAllTrueStripsScreenOnMarkFailure` |
| 23 | ревью: `ResetScreen` после `SetScreen` | `screen_msg`/`screen_round` обнулены, `updated_at` прежний | `TestResetScreenKeepsUpdatedAt` |

Не тестируется: отказ `SetScreen` после отправки (авария БД посреди `Notify`).
