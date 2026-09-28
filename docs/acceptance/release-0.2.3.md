# Выкат 0.2.3: паспорт релиза

## Состояние на 28.09.2026

- Кандидат: PR #46 `feature/alert-topic` в `prod` (срез alert-topic, galera-tasks#70),
  мердж `9d08cd603821435848c3b21a8da3e5c4b0fff9ed`.
- Чужая работа: prunable worktree `fix/prod-compose-no-proxy` (старая сессия, в `prod` не
  влит), ветки `feature/tickets-paging`, `slice-*`, `eval-base-run` без открытых PR; релиз не
  задевают. Открытых PR в `prod`, кроме #46, нет.
- Версия `v0.2.3`, patch: служебное изменение, нового поведения при пустой переменной нет.
- Приёмка среза: ревью `code-reviewer` без must-fix; паспорта среза нет - видимого
  поведения в этом релизе нет, доставка в тему проверяется на переключении (спека, «Выкат» 4-6).
- Прод до выката: `a6885b83ca51766c5d8d321b9cf41caa4eb1766a` (`v0.2.2`), схема 12.
- Что едет пользователю: ничего видимого. Код умеет слать тревогу в тему группы
  (`ALERT_THREAD_ID`), переменная заводится пустой.

## Развилки

| Развилка | Значение |
| --- | --- |
| Миграции, бэкфилл, числа наружу | нет |
| Compose | менялся: одна строка `ALERT_THREAD_ID: ${ALERT_THREAD_ID:-}`; `docker compose config` prod и кандидата различаются ровно ей |
| Новые переменные | `ALERT_THREAD_ID`, заводится пустой из Compose при `sync-compose`; `ALERT_BOT_TOKEN`, `ALERT_CHAT_ID` не трогаются |
| Кандидат | PR #46 |
| Пользовательские изменения | нет: релиз закрывается без анонса (G9 не проводится) |
| Откат | schema-compatible: `release a6885b83ca51766c5d8d321b9cf41caa4eb1766a` |

Вне релиза по решению владельца: шаги 1 и 4-6 раздела «Выкат» спеки
`docs/plans/plan-alert-topic.md` (секрет бота тревог, чат группы, тема 5, проверка
доставки). Тема при старом чате (личка) дала бы 400 на каждой тревоге.

## Ворота до выката

| Ворота | Состояние |
| --- | --- |
| G0 | команда владельца «катим тревоги», 28.09 07:00 МСК: intake, прод, v0.2.3 |
| G1 | dev-контура нет; замещающее доказательство - тесты рубежей и `compose config` (ниже) |
| G2 | `make ci-check RUN='.'` на `cb83091` с чистой базой (миграции 1-12, тесты без кеша, 0 skip, vet, lint 0 issues, build) - ok; после неё только правки документов |
| G3 | `sha-9d08cd6...` собран, `docker manifest inspect` - index amd64 |
| G4 | миграций нет; база под ночным дампом хоста |
| G5 | Compose менялся, `sync-compose` первый в `release` |
| G6 | 07:06 и 07:14 МСК: `job-errors` только августовские (2 шт.), последнее обращение 23.09, живых нет |
| G7 | тег `v0.2.3` на `9d08cd6`, push сделан |

Завершение среза по `pre-release.md` закрыто 28.09 исполнителем релиза: хвостов кода нет,
живых прогонов во внешних системах срез не делал, спека и дифф сверены, `AGENTS.md` и
`docs/contracts.md` дополнены, CHANGELOG закрыт разделом 0.2.3.

## Окно выката

Проверены очередь работ и обращения: новых упавших работ нет, живых интервью нет.
Расписаний отправок у intake нет, кроме `watch_tick` раз в 5 минут (чтение GitHub).
Выкат 07:15 МСК, до рабочего дня.

## Порядок

```sh
gh pr merge 46 --merge && git pull --ff-only
git tag -a v0.2.3 -F <тело> 9d08cd6 && git push origin v0.2.3
CONFIRM=yes coolify-deploy.sh intake build 9d08cd603821435848c3b21a8da3e5c4b0fff9ed
docker manifest inspect ghcr.io/daniil4545/tg-intake:sha-9d08cd603821435848c3b21a8da3e5c4b0fff9ed
CONFIRM=yes coolify-deploy.sh intake release 9d08cd603821435848c3b21a8da3e5c4b0fff9ed
coolify-deploy.sh intake list-env | grep ALERT_THREAD_ID
```

Workflow `Deploy` из спеки не используется: GitHub Actions у intake сняты 28.08, канон -
`coolify-deploy.sh` (`infra/intake-prod.md` базы).

## G8. Приёмка - PASS, 28.09 07:21 МСК

1. `app_revision` = `9d08cd603821435848c3b21a8da3e5c4b0fff9ed`, в `inventory` app и migrate
   на `sha-9d08cd6...`.
2. `app` и `postgres` healthy, `migrate` Exited (0); за 6 минут один `bot_started`,
   перезапусков нет.
3. Логи старта без ошибок и WARN: `db_connected`, `openrouter_ready`, `github_read_ok` и
   `github_write_ok` по 4 проекта, `bot_started`; прежнее поведение тревог -
   `alerts_enabled on=true own_bot=true thread_id=0`. В `db-logs` ERROR/FATAL нет.
4. Stop signals: нет. `job-errors` без новых записей, `deploy-state` без незавершённых
   деплоев. `coolify-deploy.sh intake list-env`: `ALERT_THREAD_ID` заведена, пустая
   (иначе старт не дал бы `thread_id=0`).
5. Функциональная проверка - замещающая: живой заявки нет, безопасного способа нет (бот
   вне `AGENT_ALLOW_LIST` сервера `galera-tg-agent`, заявка завела бы issue в репозитории
   проекта клуба). Проверено вместо: healthcheck healthy требует свежей отметки успешного
   `getUpdates` (`cmd/intake/main.go`, `PollerAlive`), то есть бот принимает апдейты;
   три `watch_tick` после старта - очередь и чтение GitHub работают; приём заявки код
   релиза не трогает (дифф - `Notify` и конфиг), путь уведомления без темы покрыт
   `TestNotifyAlertThread/no_thread`.

## G9. Анонс

Не публикуется: служебный релиз без видимых клубу изменений. Секция `### Анонс` в
разделе 0.2.3 `CHANGELOG.md` отсутствует сознательно, раздел помечен «анонса нет».

## Закрытие

- GitHub Release `v0.2.3`, `Latest`.
- galera-tasks#70 открыт: комментарий о выкате; переключение на бота тревог (шаги 1, 4-6
  «Выката» спеки) - после секрета владельца.

## Остаток риска

- Приём заявки вживую после выката не прогнан (см. G8.5).
- Доставка в тему 5 не проверена - она вне этого релиза.
- `CHANGELOG.md` 31 КБ при лимите 8 КБ, унаследовано; сжатие без потери строк «Решение:»
  требует переноса в `docs/architecture.md`, отдельная задача.
