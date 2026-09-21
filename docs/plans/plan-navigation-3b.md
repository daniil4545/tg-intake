# Сценарии проверки среза «навигация и нижняя панель»

Раздел 3b [plan-navigation.md](plan-navigation.md). Обвязка - `screen_harness_test.go` срезов
4 и 5 (фейковый Telegram пишет метод, `message_id`, `reply_markup`, текст toast; `Bot` без
`NewBot`). Срез добавляет:
- в `screenBot` поле `tickets` (`Tickets` над той же БД: `Cancel` и `Page` GitHub не зовут);
- в фейк счёт `answerCallbackQuery` на один callback: у каждого нажатия в тестах ровно один
  ответ, два ответа - провал теста.

Панель - поле `keyboard` в `reply_markup`, экран - `inline_keyboard`. Проект `p`, автор 1.
«Stale» ниже - факт: один `answerCallbackQuery` с текстом «Этот экран устарел»,
`editMessageReplyMarkup(id)` нажатого без клавиатуры, `sendMessage` нет, новых событий и работ нет.

## Карта входов

| Вход | Класс | Что шлём |
|---|---|---|
| `/start`, «Меню», свободный текст без обращения | навигация | `homeScreen(intro)`: одно сообщение |
| «Сброс» без обращения; сброс пустого сбора; «Да, сбросить»; «Закончить разговор» | переход | прежняя правка нажатого (где была), `sendPanel(intro)`, `homeScreen("")` |
| «Готово» без обращения | переход | одно сообщение с `homeKeyboard()` |
| исход отмены тикета | навигация | правка `cs.Screen`; 0 - новым сообщением |
| устаревшая или незнакомая кнопка | отклик | `staleButton` |

`finishKill(ctx, cs, text)`: `projectOf(cs)` - nil или ошибка: ошибка наверх, работа в повтор;
разметка `backToList(slug, 0)`; `cs.Screen == 0` - `Info kill_screen_missing`, `sendLong`;
иначе `editScreen(user, StoredMessage{cs.Screen, cs.UserID}, text, markup)`.

## Сценарии

| # | Дано / когда / тогда | Источник риска | Факт проверки | Ярус | Тест |
|---|---|---|---|---|---|
| 1 | нет активного обращения, два проекта; `/start`, «Меню», текст «привет» | правило 4: панель шла вторым сообщением | на каждый вход ровно один `sendMessage`: `inline_keyboard` - два проекта и «Добавить проект», `keyboard` нет, текст оканчивается `homeText` | БД | `TestHomeScreenOneMessage` |
| 2 | «Сброс» без обращения; «Сброс» при пустом сборе | переход без панели оставил бы «Готово» | ровно два `sendMessage`: первый с `keyboard` [[«Меню», «Сброс»]] без `inline_keyboard`, второй - домашний экран без `keyboard` | БД | `TestTransitionSendsPanel` |
| 3 | «Да, сбросить» с id 50 при сборе с материалом; «Закончить разговор» с id 60 после ответа по документации | то же, плюс правка нажатого | `editMessageText` нажатого раньше отправок, затем два `sendMessage`, как в строке 2 | БД | `TestTransitionSendsPanel` |
| 4 | обращений нет, «Готово» (панель сбора после `SweepDrafts`) | застрявшая панель без «Меню» | один `sendMessage` с `keyboard` [[«Меню», «Сброс»]] | БД | `TestTransitionSendsPanel` |
| 5 | опубликованное #7; «Отменить тикет» `p:7` с id 300; новый `Bot`; `Notify` `keysCancel` «Тикет #7 отменён и закрыт.» | Р-8: `kills` в памяти терял экран | после нажатия `screen_msg = 300`, работа `cancel_issue`; после `Notify` - `editMessageText(300)` с текстом исхода и одной кнопкой `tickets`, data `p:0`; `sendMessage` нет | БД | `TestKillOutcomeOnCard` |
| 6 | то же, `Notify` второй раз | повтор работы | вторая правка 300, `sendMessage` нет | БД | `TestKillOutcomeOnCard` |
| 7 | `screen_msg = 0` (отмена до среза); `Notify` `keysCancel` | молчание вместо исхода | один `sendMessage` с текстом исхода и «К списку» `p:0` | БД | `TestKillOutcomeOnCard` |
| 8 | `screen_msg = 300`, `editMessageText(300)` отвечает 400 `message to edit not found` | правило 2 | `sendMessage` с текстом и «К списку», затем `editMessageReplyMarkup(300)`; `Notify` вернул nil | БД | `TestKillOutcomeOnCard` |
| 9 | опубликованное A с `screen_msg = 300`, активное B в `interview` с `screen_msg = 100`; `Notify` `keysCancel` по A | исход трогает разговор | вызовы только с id 300; у B `screen_msg = 100`; `keyboard` нет ни в одном вызове | БД | `TestKillOutcomeKeepsActiveCase` |
| 10 | тикет автора 1, «Отменить тикет» жмёт автор 2 с id 300 | чужое нажатие пишет экран | `screen_msg` прежний (0), `cancel_issue` нет, `editMessageText(300)` с отказом | БД | `TestKillScreenOnlyOnCancel` |
| 11 | «К проектам» с id 50, `editMessageText(50)` отвечает 400 `message can't be edited` | правило 2: старые кнопки оставались живыми | `sendMessage` с домашним экраном, затем `editMessageReplyMarkup(50)`; лог `screen_edit_failed` | БД | `TestNavigationEditFallback` |
| 12 | то же, правка отвечает `message is not modified` (оба варианта текста Bot API) | двойное нажатие давало дубль | `sendMessage` и `editMessageReplyMarkup` нет | БД | `TestNavigationEditFallback` |
| 13 | `editScreen` с id 50, текст 4500 рун, разметка с одной кнопкой | длинный экран правкой не помещается | `editMessageText(50)` нет; два `sendMessage`, `reply_markup` только у второго; `editMessageReplyMarkup(50)`; возвращено второе сообщение | БД | `TestNavigationEditFallback` |
| 14 | `summary`, `screen_msg = 100`; «Поправить» с id 100, `editMessageText(100)` отвечает 400 | M2: новый экран не записан, «Публикую» на нём - stale | `sendMessage` с «Публикую»; `screen_msg` = id этого сообщения | БД | `TestFixFallbackSetsScreen` |
| 15 | активное обращение в `interview`, `screen_msg = 100`; «К проектам» и «Назад» с id 50 | навигация закрывала бы живой экран | `editMessageReplyMarkup(100)` нет; `screen_msg = 100` | БД | `TestNavigationKeepsLiveScreen` |
| 16 | «Всё так» с id 100 = `screen_msg`: data `x`; раунд 2 при data `1` (`ErrStaleRound`); статус `summary` (`ErrNotInterview`) | M1: второй ответ на callback, текстовый отказ | stale по id 100; `answer_given` нет | БД | `TestStaleButtonRefused` |
| 17 | «Открыть тикет» и «Отменить тикет» с data `p` и `p:x`, id 40 | битые data отвечали сообщением | stale по id 40; `cancel_issue` нет | БД | `TestStaleButtonRefused` |
| 18 | «Да, сбросить» с data чужого обращения, id 50 | подтверждение старого обращения | stale по id 50; обращение в прежнем статусе | БД | `TestStaleButtonRefused` |
| 19 | `staleButton` на callback `\frestart|` (кнопка прежней версии) с id 70 | вечный спиннер или сообщение-отказ | stale по id 70 | быстрый | `TestUnknownButtonStale` |

Строка 13 зовёт `editScreen` напрямую: длинный экран через хендлер требует карточки из GitHub.
Строка 19 зовёт `staleButton` напрямую: маршрут `tele.OnCallback` в `NewBot` тестом не
покрыт, `NewBot` поднимает поллер.

Не тестируется: отказ записи `screen_msg` в `Cancel` - откатывается вся транзакция, путь тот
же, что у отказа `Cancel`.

Имена для §7: `TestHomeScreenOneMessage TestTransitionSendsPanel TestKillOutcomeOnCard
TestKillOutcomeKeepsActiveCase TestKillScreenOnlyOnCancel TestNavigationEditFallback
TestFixFallbackSetsScreen TestNavigationKeepsLiveScreen TestStaleButtonRefused
TestUnknownButtonStale`.
