# Состояние проекта

Задача: galera-tasks#32, свободная форма тикета и единый стиль кнопок | Шаг: все 8 срезов в
`feature/ticket-form`, PR #43 в `prod` открыт, релиз 0.2.0 не выкачен | Блокер: нет | От
владельца: команда на прод-релиз, гейт B - issue #10 и #11 в песочнице.

Обновлено: 2026-09-22. В проде: 0.1.13 (2026-08-20, ревизия `2a99dfb`), dev-контура нет.
Ветка функции: `feature/ticket-form`, PR в `prod` - #43 (открыт). Спека -
[specs/ticket-form.md](specs/ticket-form.md), приложение -
[specs/ticket-form-appendix.md](specs/ticket-form-appendix.md). Порядок работ -
[backlog.md](backlog.md), решения - [architecture.md](architecture.md), паспорт приёмки -
[acceptance/ticket-form.md](acceptance/ticket-form.md).

## Сделано

- 21.09, глобальная спека #32 (фаза Spec), `spec-review` два прохода pass with fixes.
- Срезы 1-8 выполнены, слиты в `feature/ticket-form` (`git cherry` пуст). Срез 8 - сквозной
  прогон в песочнице `daniil4545/intake-sandbox`: 4 прогона, финальный PASS (3/5, S2 и S5
  пропущены провайдером), `eval-compare` pass. Паспорт -
  [acceptance/ticket-form.md](acceptance/ticket-form.md).
- PR #43 `feature/ticket-form` в `prod` открыт, тикет `status:dev`.

## В работе

Релиз 0.2.0 не выкачен, ждёт команды владельца. Причины:
(а) в ветке `prod` compose 0.1.14 рассчитан на узел A без пина `api.telegram.org`, прод ещё
на старом VPS - выкат синхронизирует compose и Telegram отвалится (наблюдение 08.08); правку
compose (вернуть пин с пометкой «снять при переезде») классификатор auto mode агенту не дал
сделать;
(б) прод-выкат - только по команде владельца.

## Дальше

1. Гейт B: владелец читает issue #10 и #11 в песочнице (паспорт приёмки, раздел «Гейт B»).
2. Компромисс по compose-пину `api.telegram.org` - решение владельца или ожидание переезда
   на узел A.
3. Релиз 0.2.0 - скилл `release` по команде владельца.

Следом по бэклогу: #2 (тикеты в galera-tasks), #31 (ответ на комментарий из бота).

## Открытое

- Провайдер модели медленнее `llmTimeout` 60 с на заметной доле ходов - в прогонах среза 8
  по 7 и 8 таймаутов на обращение (`docs/acceptance/ticket-form.md`).
- Значение `INTERVIEW_ROUNDS` в контуре не сверено с новым дефолтом 2.
- Спека функции в `feature/ticket-form`, в `prod` её нет до мерджа PR #43.
