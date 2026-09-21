# Спека среза: ядро контракта и свободная форма

Статус: agreed
Issue: galera-tasks#32, срез 2 из 8
Каноны: `docs/specs/ticket-form.md` (§5, §8, R1-R4, R6, Р-1..Р-7, Р-14), `docs/llm.md`
Architecture review: pass with fixes, находки закрыты текстом

## 1. Цель и границы

- Цель: Go проверяет только ядро типа, знает `mixed`, принимает разделы со свободными
  заголовками, пишет «Не уточнено» вместо «Не разобрано».
- Результат: команды §7 зелёные; обращения в полёте переживают `0011`.
- Делаем: правила, `checkTurn` с `detail`, саммари, тело и метки issue, строку саммари
  автору, дефолт `INTERVIEW_ROUNDS=2` (Р-4), `0011`, канон.
- Промты - только без чего код не работает: «Ответ» обоих (схема), в `interview.md` -
  `mixed`, обязательность, порядок вопросов по ядру; «Разделы» `summary.md`. Приёмы
  `detail` и тон - срез 3.
- Не делаем: живой экран, «Отправить как есть», `texts.go` (срезы 4-7), eval.

## 2. Архитектура

| Блок | Изменение |
|---|---|
| `rules/contract.json` | ядро Р-1 и `mixed`; поле `required` уходит |
| `contract.go` | `mixed` в `caseKinds`; `Prompt` без метки обязательности; `Unclear` |
| `interview.go` | `checkTurn` с `detail`, `dropDetails`, `Section`, `checkSummary` по Р-6 |
| `github.go` | `typeLabels`, строка `Unclear` вместо «## Не разобрано» |
| `0011_core_contract.sql` | CHECK `kind` с `mixed`, перевод ключей Р-14 |

## 3. Сценарии

R1-R4, R6 - в §3b. Неверный ход или саммари - один повтор, затем очередь. `bug -> mixed`
сохраняет `case`, `wrong`.

## 3a. Рубежи молчания

| Состояние | Чего не делаем | Чем держится | Тест |
|---|---|---|---|
| `published`, `cancelled`, `collecting`, `answering` | `0011` не трогает | `WHERE status IN` | `TestMigration0011` |
| любое | `0011` не меняет `status`, `updated_at`, не ставит работ | пишет 3 колонки | `TestMigration0011` |
| ядро закрыто | нет `incomplete` и «Не уточнено» | `Missing` пуст | `TestIssueBodyUnclear` |

## 3b. Сценарии проверки

| # | Дано / когда / тогда | Факт | Ярус | Тест |
|---|---|---|---|---|
| 1 | R1: ход `mixed` с 4 ключами принят; ключ `expected` отклонён | ошибка `checkTurn` | быстрый | `TestCheckTurn` |
| 2 | R2: `detail` при `gaps=[]` - принят; `detail` при `ready`, только `detail` при открытом ядре, вопрос про закрытый ключ - отклонены | то же | быстрый | `TestCheckTurn` |
| 3 | R6: два `detail` при `round=0` - остаётся первый; при `round=1` снят | вопросы | быстрый | `TestDropDetails` |
| 3a | правка саммари при `round=0`, модель дала два `detail` | `round_asked` с одним вопросом | БД | `TestFixKeepsOneDetail` |
| 4 | R6: `round=1`, модель вернула только `detail` | `interview_done`, работа саммари, `notify` нет | БД | `TestDetailOnlyGoesToSummary` |
| 5 | R3: разделов 0 и 6 - ок; 7, заголовок 61 символ, с `\n`, `#`, `<`, «Кратко», « ссылки», пустой текст, чужой `key` - ошибка | ошибка `checkSummary` | быстрый | `TestCheckSummary` |
| 6 | `sections=[]`, закрыты `case`, `wrong` | тело `## Конкретный случай`, `## Что пошло не так` | быстрый | `TestSectionsFallBackToContract` |
| 6a | раздел с `key=case`, закрытый `wrong` не покрыт разделом | `## Что пошло не так` дописан из `filled` | быстрый | `TestSectionsKeepCore` |
| 7 | телефон в заголовке; раздел с `key` из `gaps` | `[телефон]`, раздел остался | быстрый | `TestRenderSections` |
| 7a | ядро закрыто / открыто / `question` без ядра и разделов | `incomplete`, «Не уточнено» в `notify`, тело из протокола | БД | `TestSummarizeUnclear` |
| 8 | R4: `gaps=[case]`; `mixed` с открытыми `case`,`need`; всё закрыто | «---», «Не уточнено: конкретный случай.» перед маркером; «...случай, что нужно.»; строки и «Не разобрано» нет | быстрый | `TestIssueBodyUnclear` |
| 9 | R1: публикация `mixed` с открытым `case` | тело POST `/issues` в стабе: `type:bug`, `type:feature`, `incomplete` | БД | `TestPublishMixedLabels` |
| 10 | саммари при открытом `wrong` | «Не уточнено: что пошло не так.», «Остались пробелы» нет | быстрый | `TestSummaryMessageUnclear` |
| 11 | Р-14: `UpTo(10)`, строки §4 в `interview`, `summary`, `publishing`; `interview` до хода (`kind` NULL, `{}`); коллизия `result`+`need`; контроль `published`, `collecting`, `answering`; `UpTo(11)` | `contract`, `gaps`, `incomplete`, `updated_at` строк | БД | `TestMigration0011` |
| 12 | up на уже новых ключах | ключи не изменились | БД | `TestMigration0011` |
| 13 | down: `mixed` стал `bug`, `wrong` снят из `gaps`; вставка `mixed` падает CHECK | `kind`, `gaps`, ошибка | БД | `TestMigration0011` |
| 14 | `contract.json` без `mixed` | ошибка `LoadContract` | быстрый | `TestLoadContract` |

`TestMigration0011` в `Cleanup` поднимает схему до последней версии. Старые тесты - на ядро.

## 4. Данные и состояния

- `kind`: CHECK с `mixed`. `contract`, `gaps` - ключи ядра; `gaps` равен `Missing`:
  `checkTurn` требует пустой ключ в `gaps`, `mergeFilled` снимает `gaps` из `filled`.
- `ready` = `gaps` пуст и вопросов нет; вопрос - ключ из `gaps` или `detail`.

`0011` Up в `interview`, `summary`, `publishing`, пишет `contract`, `gaps`, `incomplete`:

| Старое | `contract` | `gaps` |
|---|---|---|
| `expected`, `actual` | `wrong` = непустые через `\n`, если ни одного нет в `gaps` | `wrong`, если там любой |
| `result`, `problem` | `need`, `why` | `need`, `why` |
| `case`, `question` | сохраняются | сохраняются |
| `wrong`, `need`, `why` | сохраняются, старый ключ их не перезаписывает | сохраняются |
| прочие | отброшены | отброшены |

Ключ из `contract` в `gaps` не попадает. `incomplete = gaps непуст` в `summary`, `publishing`.
Down: `mixed` -> `bug`, CHECK назад, новые ключи снимаются в живых статусах.

## 5. Кодовая модель

```go
var caseKinds = []string{"bug", "feature", "question", "mixed"}
type ContractItem struct{ Key, Title string } // все пункты - ядро (Р-7)
const detailKey = "detail"                    // уточнение вне ядра (Р-3)
type Section struct{ Key, Heading, Text string } // Key - пункт ядра или ""

func (c Contract) Unclear(kind string, filled map[string]string) string
func dropDetails(questions []Question, round int) (kept []Question, dropped int)
func typeLabels(kind string) []string
```

- `Unclear`: названия `Missing` с первой строчной через «, » в «Не уточнено: ...»; ядро
  закрыто - "". Одна функция для саммари и тела.
- `dropDetails`: при `round == 0` (`cs.Round` до хода) остаётся первый `detail`, иначе
  снимаются все; в `Run` после отсева исчерпанных, и при правке; лог `detail_dropped`.
- `checkTurn`: `detail` минует «ключ из `gaps`» и «повтор ключа»; не `ready` при
  непустом `gaps` - нужен вопрос по ключу из `gaps`.
- `checkSummary`: разделов до 6; `key` пуст или пункт ядра типа; заголовок одной строкой,
  1-60 символов, без `#`, `<`, не «Кратко», «Ссылки», «Пересечения» (`EqualFold` после
  `TrimSpace`), текст непустой и без строк на `#`.
- `renderSections`: разделы в порядке модели; закрытый пункт ядра без своего раздела - из
  `filled`; тело пусто - раздел из `cs.Protocol` (решение 7). `scrubContacts` везде.
- `typeLabels`: `mixed` - `type:bug`, `type:feature`.

## 6. Этапы реализации

1. Тесты §3b, красные. 2. `contract.json`, `contract.go`, дефолт раундов, `.env.example`.
3. Интервью, промты §1. 4. Тело, метки. 5. `0011`. 6. Канон: `llm.md`, `AGENTS.md`,
`prd.md`, `architecture.md`.

## 7. Критерий приёмки

- `make -C src ci-check DATABASE_URL=<лок> TEST_DATABASE_URL=<лок>` - аргументами make:
  `src/.env` перекрыл бы окружение.
- `TEST_DATABASE_URL=<лок> go test ./internal/app -run '<тесты §3b>' -v -count=1` из `src`,
  в выводе нет `--- SKIP`.
- Не автоматизируется: качество разделов (R3 «сделки один раз») - срез 3 и гейт B.

## 8. Обязательный хвост среза

| Шаг | Что именно | Отметка |
|---|---|---|
| Триаж | #32 в работе | in-progress |
| Регрессор | `regress`: тело, метки, `incomplete`, `0011` на живых статусах | |
| Ревью и PR | `code-reviewer`, коммит в `feature/ticket-form` | |
| Полный прогон | команды §7 | |
| Журнал | `docs/state.md`; Р-17 - в очередь `kb-sync` | |
| Релиз функции | `INTERVIEW_ROUNDS` контура (`safe-ssh.sh deploy-config`, только имена); порядок остановки старого app и `migrate` в Coolify - по runbook | |

## 9. Решения среза (8)

1. Пустые `sections` - сразу из ядра, без повтора (решение 2026-08-09,
   `TestSummaryWithoutSections`). Р-6 глобальной спеки поправлен.
2. `0011` переводит и `publishing` (может ждать выката) и пересчитывает `incomplete` (у
   `feature` были обязательны `today`, `done`); новый ключ старым не перезаписывается.
3. `required` удаляется: флаг, всегда true, - мёртвые данные.
4. «Не уточнено» из `Missing`, а не из `gaps` модели: счёт Go, равенство - §4.
5. База среза 1 снимается на коде без среза 2; влит раньше - worktree на `prod`.
6. Р-17 - через `kb-sync`: базу по ходу среза правит только он.
7. Ревью кода (диспетчер): раздел с `key` из `gaps` не снимается, пробел назовёт «Не
   уточнено»; пустое тело - из протокола, а не ошибка.
8. Метка `incomplete` при публикации - из `Unclear`, как строка тела, а не `cs.Incomplete`.
