//go:build live

package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tele "gopkg.in/telebot.v4"
)

// live_test.go - срез 8 ticket-form (docs/plans/plan-live-run.md): сквозной
// прогон диалогов через настоящие хендлеры бота и воркер. Telegram - фейковый
// (fakeTelegram/screenBot из screen_harness_test.go), OpenRouter и GitHub -
// настоящие, песочница daniil4545/intake-sandbox. Маршруты NewBot тестом не
// покрыты (§9 плана): хендлеры зовутся напрямую, как в остальных тестах пакета,
// а не через tb.ProcessUpdate - таблица кнопка->хендлер лежит в голове теста,
// а не в коде.
//
// Модель недетерминирована: сценарии проверяют факты исхода (issue, метки,
// contract, gaps, события), а не дословные тексты - кроме toast'ов «Принято» и
// «Этот экран устарел», зафиксированных §11 глобальной спеки и не тронутых
// срезом 7 (texts.go).

const (
	// Единственная база, с которой работает live: TRUNCATE в начале не должен
	// иметь шанса задеть что-то ещё.
	liveHost     = "localhost"
	livePort     = uint16(5434)
	liveDatabase = "intake_live"
	// Проект зашит, а не читается из окружения: второе условие защиты (§2 плана)
	// выполняется самим устройством теста - другой репозиторий взять неоткуда.
	liveOwner        = "daniil4545"
	liveRepoName     = "intake-sandbox"
	liveProjectSlug  = "sandbox"
	liveProjectTitle = "Qualifier (песочница)"
	liveAuthorID     = int64(1)
	liveRounds       = 2
	liveMaxItems     = 30
	// Дедлайн одного шага (§3 плана): ход модели, публикация в GitHub. Не
	// меньше двух подряд попыток работы воркера (jobTimeout каждая, см.
	// worker.go) с паузой между ними - первая живая попытка на S4 упёрлась в
	// более короткий предел и упала до второй попытки, хотя продукт был ни
	// при чём.
	liveStepDeadline = 2*jobTimeout + 30*time.Second
	livePoll         = 2 * time.Second
	// Запас перед дедлайном самого теста (t.Deadline, из -timeout): без него
	// зависший шаг ловит не наш Fatalf, а -timeout убивает процесс мимо
	// t.Cleanup - отменённое обращение и открытый issue в песочнице остались бы
	// висеть.
	liveDeadlineMargin = 30 * time.Second
)

// liveContext - контекст проекта для промта интервью. eval/cases.jsonl (Р-10
// ticket-form) не читается: набор untracked и в этом дереве отсутствует
// (worktree отдельный от того, где его выгружали). Вместо реального контекста
// qualifier - краткое собственное описание того же домена, которого достаточно
// модели для интервью; решение записано в отчёте среза 8.
const liveContext = `Qualifier - бот-квалификатор лидов клуба поверх Bitrix24: ` +
	`встречает нового лида, ведёт диалог по сценарию, ставит сделку и её статус, ` +
	`шлёт напоминания о встречах. Пример сущностей: сделка (deal), лид, статус ` +
	`сделки, напоминание.`

type liveEnv struct {
	ft      *fakeTelegram
	tb      *tele.Bot
	b       *Bot
	cases   *Cases
	gh      *GitHub
	project Project
}

func TestLiveRun(t *testing.T) {
	dsn := requireLiveDatabase(t)
	openrouterKey := requireEnv(t, "OPENROUTER_API_KEY")
	githubToken := requireEnv(t, "GITHUB_TOKEN")

	ctx := context.Background()
	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx,
		`TRUNCATE cases, case_items, case_events, jobs, users, projects RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// Info, не Warn: разбор живого прогона нужен interview_round (gap_keys) и
	// llm_call - оба уровня Info.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := SyncProjects(ctx, pool, []ProjectConfig{{
		Slug: liveProjectSlug, Title: liveProjectTitle,
		Owner: liveOwner, Repo: liveRepoName, Context: liveContext,
	}}, log); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	projects, err := ListProjects(ctx, pool)
	if err != nil || len(projects) != 1 {
		t.Fatalf("load seeded project: projects=%v err=%v", projects, err)
	}
	project := projects[0]

	media, err := NewMedia(t.TempDir(), log)
	if err != nil {
		t.Fatalf("new media: %v", err)
	}
	rules, err := LoadContract()
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	statuses, err := LoadStatuses()
	if err != nil {
		t.Fatalf("load statuses: %v", err)
	}

	proxy := os.Getenv("OPENROUTER_PROXY")
	if err := checkProxy("OPENROUTER_PROXY", proxy); err != nil {
		t.Fatal(err)
	}
	model := valueOr(os.Getenv("OPENROUTER_MODEL_DIALOG"), "deepseek/deepseek-v4-flash-0731")
	reasoning, err := parseReasoning(valueOr(os.Getenv("OPENROUTER_REASONING_DIALOG"), "low"))
	if err != nil {
		t.Fatal(err)
	}
	dialog := DialogModel{Name: model, Reasoning: reasoning}

	cases := NewCases(pool, media, log, liveMaxItems, 0)
	llm := NewOpenRouter(openrouterKey, dialog.Name, proxy, log)
	// GITHUB_TOKEN живёт только в этой переменной и в клиенте: в лог и в код
	// он не идёт нигде дальше.
	gh := NewGitHub(githubToken, GitHubAPI, statuses, log)
	overlap := NewOverlap(gh, llm, log, dialog)
	interview := NewInterview(cases, llm, log, rules, dialog, liveRounds, overlap)
	publisher := NewPublisher(cases, gh, rules, log, 0)
	normalizer := NewNormalizer(cases, llm, log)
	ticketsSvc := NewTickets(cases, gh, statuses, log, 0)
	projectsSvc := NewProjects(cases, gh, llm, dialog, log)
	lookup := NewLookup(cases, gh, llm, log, dialog)

	ft, tb := newFakeTelegram(t)
	b := screenBot(tb, pool, cases, log, ticketsSvc)
	b.projects = projectsSvc
	b.allowed = []int64{liveAuthorID}

	handlers := map[string]JobHandler{
		JobNormalizeVoice:  normalizer.RunNormalizeVoice,
		JobNormalizeImage:  normalizer.RunNormalizeImage,
		JobFinishNormalize: normalizer.RunFinishNormalize,
		JobInterview:       interview.Run,
		JobSummarize:       interview.Summarize,
		JobPublish:         publisher.Run,
		JobNotify:          b.Notify,
		JobCancelIssue:     ticketsSvc.RunCancel,
		JobLookup:          lookup.Run,
	}
	workerCtx, cancelWorker := context.WithCancel(ctx)
	t.Cleanup(cancelWorker)
	go RunWorker(workerCtx, pool, log, handlers, cases.HandleFailedJob)

	env := &liveEnv{ft: ft, tb: tb, b: b, cases: cases, gh: gh, project: project}

	// Один автор (id 1, единственный в белом списке) ведёт сценарии по очереди:
	// активное обращение у него одно, и следующий сценарий начинается только
	// когда предыдущее опубликовано (или отменено уборкой упавшего - см.
	// startCase/cancelIfActive).
	t.Run("S1_bug_R2", func(t *testing.T) { runBugScenario(t, env) })
	t.Run("S2_feature_R2", func(t *testing.T) { runFeatureScenario(t, env) })
	t.Run("S3_mixed_R1", func(t *testing.T) {
		runMixedScenario(t, env, "S3", []string{mixedR1Text}, mixedR1Answers, false)
	})
	t.Run("S4_mixed_gate_b", func(t *testing.T) {
		runMixedScenario(t, env, "S4", mixedSecondMaterial, mixedR1Answers, false)
	})
	t.Run("S5_skip_R5", func(t *testing.T) { runSkipScenario(t, env) })
}

// requireLiveDatabase - защита §2 плана: только intake_live на localhost:5434,
// прод-адрес из .env сюда попасть не должен. Печатает хост/порт/базу, а не сам
// DSN - в нём пароль.
func requireLiveDatabase(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv("LIVE_DATABASE_URL")
	if dsn == "" {
		t.Fatal("LIVE_DATABASE_URL is not set: live truncates the database it runs on")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse LIVE_DATABASE_URL: %v", err)
	}
	host := cfg.ConnConfig.Host
	if host != liveHost && host != "127.0.0.1" {
		t.Fatalf("LIVE_DATABASE_URL host %q is not %s: live runs only against the local sandbox", host, liveHost)
	}
	if cfg.ConnConfig.Port != livePort {
		t.Fatalf("LIVE_DATABASE_URL port %d is not %d", cfg.ConnConfig.Port, livePort)
	}
	if cfg.ConnConfig.Database != liveDatabase {
		t.Fatalf("LIVE_DATABASE_URL database %q is not %q: refusing to truncate anything else",
			cfg.ConnConfig.Database, liveDatabase)
	}
	return dsn
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set", name)
	}
	return v
}

func must(t *testing.T, err error, action string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
}

// startCase - вход каждого сценария (§4 плана): /start, проект, «Создать
// тикет». Экран, правящийся до заведения обращения (домашний список, меню
// проекта), фейк не проверяет по id - фейковый Bot API не отличает
// существующее сообщение от нового, а живой экран обращения (Screen)
// появляется только вместе с самим обращением.
func startCase(t *testing.T, env *liveEnv) string {
	t.Helper()
	ctx := context.Background()

	must(t, env.b.onStart(textCtx(env.tb, liveAuthorID, "/start")), "onStart")
	pressButton(t, env, env.b.onProject, 1, env.project.Slug, "")
	pressButton(t, env, env.b.onCreate, 1, env.project.Slug, "")

	cs, err := env.cases.Active(ctx, liveAuthorID)
	if err != nil {
		t.Fatalf("active case after start: %v", err)
	}
	if cs == nil {
		t.Fatal("нет активного обращения после «Создать тикет»")
	}
	// Упавший сценарий не должен держать активное обращение автора: следующий
	// startCase получил бы его вместо нового и упал бы на чужом состоянии.
	t.Cleanup(func() { cancelIfActive(t, env, cs.ID) })
	return cs.ID
}

// cancelIfActive - уборка провалившегося сценария: CancelCase - no-op на уже
// опубликованном или отменённом обращении, так что вызов безопасен и после
// успеха сценария тоже.
func cancelIfActive(t *testing.T, env *liveEnv, caseID string) {
	t.Helper()
	cs, err := env.cases.Load(context.Background(), caseID)
	if err != nil || cs == nil {
		return
	}
	if err := env.cases.CancelCase(context.Background(), cs, "live run cleanup"); err != nil {
		t.Logf("не удалось отменить обращение %s при уборке: %v", caseID, err)
	}
}

// sendMaterial отправляет материал сценария по одному сообщению - так его
// присылал бы автор - и возвращает обращение с заведённым счётчиком (тоже
// живой экран, Р-8): от него отсчитывается первая смена экрана.
func sendMaterial(t *testing.T, env *liveEnv, caseID string, lines []string) *Case {
	t.Helper()
	for _, line := range lines {
		must(t, env.b.onItem(textCtx(env.tb, liveAuthorID, line)), "onItem")
	}
	return reload(t, env.cases, caseID)
}

func finishCollect(t *testing.T, env *liveEnv) {
	t.Helper()
	must(t, env.b.onDone(textCtx(env.tb, liveAuthorID, "Готово")), "onDone")
}

// stepDeadline - дедлайн одного ожидания: не больше liveStepDeadline и не
// позже дедлайна самого теста (t.Deadline из -timeout) минус запас на
// Cleanup - иначе завершение по -timeout прибьёт процесс мимо t.Fatalf, и
// отменить обращение да закрыть issue в песочнице будет некому.
func stepDeadline(t *testing.T) time.Time {
	t.Helper()
	deadline := time.Now().Add(liveStepDeadline)
	if td, ok := t.Deadline(); ok {
		if safe := td.Add(-liveDeadlineMargin); safe.Before(deadline) {
			deadline = safe
		}
	}
	return deadline
}

// waitForCase ждёт, пока обращение дойдёт до состояния match и его живой
// экран сменится (screen_msg отличается от prevScreen): смена экрана значит,
// что доставку взял на себя воркер (работа notify), а не только сохранил
// статус - до неё автору нечего было бы нажимать. Как только новый экран
// найден, прошлый (prevScreen) обязан быть уже снят - assertScreenStripped:
// это и есть проверяемая часть инварианта §3 «не больше одного живого
// inline-экрана» (устройство кода гарантирует, что кнопки несёт только
// текущий cases.screen_msg, остальные экраны - навигация, правящаяся на месте,
// без накопления новых сообщений с клавиатурой).
func waitForCase(t *testing.T, env *liveEnv, caseID string, prevScreen int, match func(*Case) bool) *Case {
	t.Helper()

	deadline := stepDeadline(t)
	for {
		cs := reload(t, env.cases, caseID)
		if match(cs) && cs.Screen != prevScreen {
			if prevScreen != 0 {
				assertScreenStripped(t, env.ft, prevScreen)
			}
			return cs
		}
		if time.Now().After(deadline) {
			t.Fatalf("не дождались шага обращения %s за %s: status=%s round=%d screen=%d",
				caseID, liveStepDeadline, cs.Status, cs.Round, cs.Screen)
		}
		time.Sleep(livePoll)
	}
}

// assertScreenStripped - §3 плана: прошлый живой экран обязан получить
// editMessageReplyMarkup (снятие кнопок) до того, как новый стал живым -
// иначе оба остались бы кликабельны разом, что и запрещает «не больше одного
// сообщения с inline_keyboard».
func assertScreenStripped(t *testing.T, ft *fakeTelegram, screenID int) {
	t.Helper()
	if n := len(ft.stripsOf(screenID)); n == 0 {
		t.Errorf("экран %d не снят при смене живого экрана: кнопки прошлого шага остались активны", screenID)
	}
}

// waitForRoundOrSummary - шаг §3 плана: следующий раунд вопросов либо саммари
// без раунда. Второе значение - был ли раунд.
func waitForRoundOrSummary(t *testing.T, env *liveEnv, caseID string, priorRound, prevScreen int) (*Case, bool) {
	t.Helper()
	cs := waitForCase(t, env, caseID, prevScreen, func(cs *Case) bool {
		return cs.Status == statusSummary || (cs.Status == statusInterview && cs.Round > priorRound)
	})
	return cs, cs.Status == statusInterview
}

func waitForPublished(t *testing.T, env *liveEnv, caseID string, prevScreen int) *Case {
	t.Helper()
	return waitForCase(t, env, caseID, prevScreen, func(cs *Case) bool {
		return cs.Status == statusPublished && cs.IssueNumber != 0
	})
}

// publish жмёт «Публикую», ждёт issue и сразу - пока не случилось ничего
// дальше - логирует номер и регистрирует уборку: паспорт прогона обязан
// показать номер даже если следующая проверка сценария зафейлится. closeAfter
// - S1/S2/S5 закрывают тикет в песочнице сами (§8 плана), S3/S4 оставляют
// открытым для гейта B.
func publish(t *testing.T, env *liveEnv, caseID string, screenID int, closeAfter bool) *Case {
	t.Helper()
	pressPublish(t, env, screenID)
	cs := waitForPublished(t, env, caseID, screenID)
	t.Logf("issue #%d создан в %s/%s", cs.IssueNumber, env.project.Owner, env.project.Repo)
	if closeAfter {
		t.Cleanup(func() { closeSandboxIssue(t, env, cs.IssueNumber) })
	}
	return cs
}

// buildAnswer собирает ответ автора одним текстом (§3 плана): заготовка на
// известный ключ вопроса, «Не знаю» на незнакомый.
func buildAnswer(questions []Question, answers map[string]string) string {
	if len(questions) == 0 {
		return "Не знаю"
	}
	lines := make([]string, 0, len(questions))
	for _, q := range questions {
		if a, ok := answers[q.Key]; ok && a != "" {
			lines = append(lines, a)
		} else {
			lines = append(lines, "Не знаю")
		}
	}
	return strings.Join(lines, "\n")
}

// pressButton нажимает инлайн-кнопку через хендлер бота и проверяет §3: на
// нажатие приходится ровно один answerCallbackQuery - ноль (не ответили) и
// два (двойной ответ - его отдельно ловит cleanup фейка, assertSingleAnswers)
// одинаково провал, поэтому считается прирост числа вызовов по конкретному
// нажатию, а не последний тост всего прогона. wantToast пустой - текст не
// сверяется (стиль тостов вне §11 меняет срез 7), иначе - дословно.
func pressButton(t *testing.T, env *liveEnv, handler func(tele.Context) error, screenID int, data, wantToast string) {
	t.Helper()

	before := len(env.ft.methodCalls("answerCallbackQuery"))
	must(t, handler(callbackCtx(env.tb, liveAuthorID, screenID, data)), "press")

	calls := env.ft.methodCalls("answerCallbackQuery")
	if len(calls) != before+1 {
		t.Fatalf("answerCallbackQuery на нажатие: было %d, стало %d, ожидался прирост ровно на 1", before, len(calls))
	}
	if wantToast != "" {
		got, _ := calls[len(calls)-1].body["text"].(string)
		if got != wantToast {
			t.Errorf("toast: %q, ожидалось %q", got, wantToast)
		}
	}
}

func pressSkip(t *testing.T, env *liveEnv, screenID, round int, wantToast string) {
	t.Helper()
	pressButton(t, env, env.b.onSkip, screenID, strconv.Itoa(round), wantToast)
}

func pressPublish(t *testing.T, env *liveEnv, screenID int) {
	t.Helper()
	pressButton(t, env, env.b.onPublish, screenID, "", "")
}

func fetchIssue(t *testing.T, env *liveEnv, cs *Case) Issue {
	t.Helper()
	issue, err := env.gh.GetIssue(context.Background(), env.project, cs.IssueNumber, false)
	if err != nil {
		t.Fatalf("read issue #%d: %v", cs.IssueNumber, err)
	}
	return issue
}

// assertGapConsistency - R4/Р-13: метка incomplete, строка «Не уточнено:» в
// теле и непустой gaps идут втроём или не идут вовсе; старая строка «Не
// разобрано» не должна встречаться нигде (R4).
func assertGapConsistency(t *testing.T, cs *Case, issue Issue) {
	t.Helper()

	hasGaps := len(cs.Gaps) > 0
	if hasGaps != cs.Incomplete {
		t.Errorf("incomplete=%t не совпадает с gaps=%v", cs.Incomplete, cs.Gaps)
	}
	hasLabel := slices.Contains(issue.LabelNames(), "incomplete")
	if hasLabel != hasGaps {
		t.Errorf("метка incomplete=%t, ожидалась %t (gaps=%v)", hasLabel, hasGaps, cs.Gaps)
	}
	hasLine := strings.Contains(issue.Body, "Не уточнено:")
	if hasLine != hasGaps {
		t.Errorf("строка «Не уточнено:» в теле=%t, ожидалась %t", hasLine, hasGaps)
	}
	if strings.Contains(issue.Body, "Не разобрано") {
		t.Errorf("тело несёт старую строку «Не разобрано» (R4): %s", issue.Body)
	}
}

// summaryReadyPayload - то, что кладёт событие summary_ready (interview.go,
// Summarize): sections - число разделов модели (len(out.Sections), до
// вставки Go-разделов ядра и до Publisher.body, который дописывает Кратко,
// Ссылки и Пересечения), body - собранное тело без них же. Оба поля - то, что
// нужно §10: «заголовков модели от 1 до 6, без повторов».
type summaryReadyPayload struct {
	Sections int    `json:"sections"`
	Body     string `json:"body"`
}

func lastSummaryReady(t *testing.T, cases *Cases, caseID string) summaryReadyPayload {
	t.Helper()
	var raw []byte
	err := cases.pool.QueryRow(context.Background(), `
		SELECT payload FROM case_events
		WHERE case_id = $1 AND kind = 'summary_ready'
		ORDER BY id DESC LIMIT 1`, caseID).Scan(&raw)
	if err != nil {
		t.Fatalf("read summary_ready of case %s: %v", caseID, err)
	}
	var p summaryReadyPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode summary_ready of case %s: %v", caseID, err)
	}
	return p
}

var liveHeadingRe = regexp.MustCompile(`(?m)^## (.+)$`)

// assertHeadings - §10 плана: заголовков модели от 1 до 6, без повторов.
// Читает последнее summary_ready - его body ещё без Кратко/Ссылок/
// Пересечений (их дописывает только Publisher.body при публикации), поэтому
// исключать их из подсчёта отдельно не нужно.
func assertHeadings(t *testing.T, cases *Cases, caseID string) {
	t.Helper()

	p := lastSummaryReady(t, cases, caseID)
	if p.Sections < 1 || p.Sections > 6 {
		t.Errorf("разделов модели: %d, ожидалось от 1 до 6", p.Sections)
	}
	seen := map[string]bool{}
	for _, m := range liveHeadingRe.FindAllStringSubmatch(p.Body, -1) {
		heading := strings.TrimSpace(m[1])
		if seen[heading] {
			t.Errorf("заголовок %q повторяется в теле саммари", heading)
		}
		seen[heading] = true
	}
}

// closeSandboxIssue - уборка §8 плана для S1, S2, S5: тикет в песочнице не
// нужен после прогона. Отказ не валит тест - тикет можно закрыть руками.
func closeSandboxIssue(t *testing.T, env *liveEnv, number int) {
	t.Helper()
	if err := env.gh.CloseIssue(context.Background(), env.project, number); err != nil {
		t.Logf("не удалось закрыть issue #%d в песочнице: %v (закрыть вручную)", number, err)
	}
}

func eventCounts(t *testing.T, cases *Cases, caseID string) string {
	t.Helper()
	kinds := []string{"round_asked", "questions_skipped", "answer_given", "summary_ready", "published"}
	parts := make([]string, len(kinds))
	for i, k := range kinds {
		parts[i] = k + "=" + strconv.Itoa(countEventKind(t, cases, caseID, k))
	}
	return strings.Join(parts, ",")
}

// runBugScenario - S1, R2 баг: материал закрывает case и wrong с первого
// хода, раунда не должно быть (round_asked=0).
func runBugScenario(t *testing.T, env *liveEnv) {
	caseID := startCase(t, env)
	cs := sendMaterial(t, env, caseID, []string{
		"https://crm.example.com/deal/59767187",
		"Сделка 59767187 закрылась статусом «Дублем», хотя должна была перейти " +
			"в «Встреча назначена». Так было только вчера, один раз - больше нигде не повторялось.",
	})
	prevScreen := cs.Screen
	finishCollect(t, env)

	cs, roundOccurred := waitForRoundOrSummary(t, env, caseID, 0, prevScreen)
	if roundOccurred {
		// Дальше вести нечего: сценарий проверяет именно прогон без раунда,
		// а с раундом это уже не S1.
		t.Fatalf("ожидался прогон без раунда (round_asked=0), но раунд %d случился: %+v",
			cs.Round, mustQuestions(t, env.cases, caseID))
	}
	if cs.Kind != "bug" {
		t.Errorf("kind=%q, ожидался bug", cs.Kind)
	}
	if cs.Filled["case"] == "" || cs.Filled["wrong"] == "" {
		t.Errorf("ядро не закрыто: filled=%v", cs.Filled)
	}
	if len(cs.Gaps) != 0 {
		t.Errorf("gaps=%v, ожидался пустой список", cs.Gaps)
	}
	assertHeadings(t, env.cases, caseID)

	cs = publish(t, env, caseID, cs.Screen, true)

	issue := fetchIssue(t, env, cs)
	labels := issue.LabelNames()
	if !slices.Contains(labels, "type:bug") || slices.Contains(labels, "type:feature") {
		t.Errorf("метки issue: %v, ожидался только type:bug", labels)
	}
	assertGapConsistency(t, cs, issue)

	t.Logf("S1: issue=#%d kind=%s gaps=%v labels=%v events(%s) failed=%t",
		cs.IssueNumber, cs.Kind, cs.Gaps, labels, eventCounts(t, env.cases, caseID), t.Failed())
}

// runFeatureScenario - S2, R2 пожелание: не больше одного раунда с вопросом
// detail; повторное «Отправить как есть» после ответа - toast «Этот экран
// устарел», без нового события пропуска.
func runFeatureScenario(t *testing.T, env *liveEnv) {
	caseID := startCase(t, env)
	cs := sendMaterial(t, env, caseID, []string{
		"Руками отказываю лидам не под портрет, например финансовый аутсорсинг. " +
			"Хочу, чтобы бот сам отказывал таким по скрипту и ставил сделке \"нерелевантен лид\"",
	})
	prevScreen := cs.Screen
	finishCollect(t, env)

	cs, roundOccurred := waitForRoundOrSummary(t, env, caseID, 0, prevScreen)
	if roundOccurred {
		// R2 обязателен только на числе раундов - какой именно ключ спросит
		// модель, недетерминировано; ключи - в лог, а не в жёсткую проверку.
		if cs.Round > 1 {
			t.Errorf("раундов %d, ожидался не больше 1", cs.Round)
		}
		questions := mustQuestions(t, env.cases, caseID)
		t.Logf("S2: раунд %d, вопросы %v", cs.Round, questionKeys(questions))
		roundScreen := cs.Screen
		answer := buildAnswer(questions, map[string]string{
			"detail": "Признак - деятельность вне маркетинговых агентств и студий разработки",
		})
		must(t, env.b.onItem(textCtx(env.tb, liveAuthorID, answer)), "onItem answer")

		before := countEventKind(t, env.cases, caseID, "questions_skipped")
		pressSkip(t, env, roundScreen, cs.Round, "Этот экран устарел")
		if after := countEventKind(t, env.cases, caseID, "questions_skipped"); after != before {
			t.Errorf("skip после ответа завёл событие пропуска: было %d, стало %d", before, after)
		}

		cs, _ = waitForRoundOrSummary(t, env, caseID, cs.Round, roundScreen)
	}
	if cs.Status != statusSummary {
		t.Fatalf("статус перед публикацией: %s, ожидался %s", cs.Status, statusSummary)
	}
	if cs.Kind != "feature" {
		t.Errorf("kind=%q, ожидался feature", cs.Kind)
	}
	if cs.Filled["need"] == "" || cs.Filled["why"] == "" {
		t.Errorf("ядро не закрыто: filled=%v", cs.Filled)
	}
	assertHeadings(t, env.cases, caseID)

	cs = publish(t, env, caseID, cs.Screen, true)

	issue := fetchIssue(t, env, cs)
	labels := issue.LabelNames()
	if !slices.Contains(labels, "type:feature") || slices.Contains(labels, "type:bug") {
		t.Errorf("метки issue: %v, ожидался только type:feature", labels)
	}
	assertGapConsistency(t, cs, issue)

	t.Logf("S2: issue=#%d kind=%s gaps=%v labels=%v events(%s) failed=%t",
		cs.IssueNumber, cs.Kind, cs.Gaps, labels, eventCounts(t, env.cases, caseID), t.Failed())
}

// mixedR1Text - дословный пример R1 из §10 глобальной спеки.
const mixedR1Text = "Напоминание пришло с неверным склонением имени, и заодно " +
	"пусть напоминание уходит за час, а не за день."

// mixedR1Answers - заготовки на case, wrong и why, которых материалу не
// хватает: see S3/S4 таблицы §4 плана среза. need закрывается материалом
// («пусть напоминание уходит за час») без отдельного вопроса.
var mixedR1Answers = map[string]string{
	"case":  "Вчера, напоминание о встрече",
	"wrong": `Имя пришло как "Анны" вместо "Анна"`,
	"why":   "Чтобы клиент не забыл: за день забывают",
}

// mixedCoreOrder - порядок ядра смеси, как в rules/contract.json.
var mixedCoreOrder = []string{"case", "wrong", "need", "why"}

// mixedAnswer - раунд смеси отвечается всеми заготовками ядра разом, если
// хоть один вопрос назвал ключ ядра: модель раунда спрашивает под одним
// ключом (например case) то, что на деле относится к другому (wrong), и
// ответ строго по ключу конкретного вопроса терял бы эту идею. Одним текстом
// (§3 плана), как и обычный buildAnswer.
func mixedAnswer(questions []Question, coreAnswers map[string]string) string {
	for _, q := range questions {
		if _, ok := coreAnswers[q.Key]; !ok {
			continue
		}
		lines := make([]string, 0, len(mixedCoreOrder))
		for _, key := range mixedCoreOrder {
			if a := coreAnswers[key]; a != "" {
				lines = append(lines, a)
			}
		}
		return strings.Join(lines, "\n")
	}
	return buildAnswer(questions, coreAnswers)
}

// mixedSecondMaterial - S4, вторая смесь для гейта B: тот же случай, что и
// mixedR1Text, но продиктованный (не переписанный дословно), плюс отдельная
// вторая идея без своего пункта ядра - проверяет, что бот не роняет её
// (инвариант «Ни одна идея не выбрасывается»), а не собирает ответ по ней.
var mixedSecondMaterial = []string{
	"Слушай, вот что было: напоминание пришло с неверным склонением имени, " +
		"спутало Анну с Анной, и вдобавок неплохо бы, чтобы оно уходило за час " +
		"до встречи, а не за день, как сейчас.",
	"И заодно пусть бот при закрытии сделки пишет причину комментарием в сделку.",
}

// runMixedScenario - S3 и S4: смесь бага и пожелания, issue с двумя метками
// типа. Issue остаётся открытым - его читает владелец на гейте B (§7 плана,
// не автоматизируется).
func runMixedScenario(t *testing.T, env *liveEnv, name string, material []string,
	answers map[string]string, closeAfter bool) {
	caseID := startCase(t, env)
	cs := sendMaterial(t, env, caseID, material)
	prevScreen, priorRound := cs.Screen, 0
	finishCollect(t, env)

	for {
		var roundOccurred bool
		cs, roundOccurred = waitForRoundOrSummary(t, env, caseID, priorRound, prevScreen)
		if !roundOccurred {
			break
		}
		questions := mustQuestions(t, env.cases, caseID)
		t.Logf("%s: раунд %d, вопросы %v", name, cs.Round, questionKeys(questions))
		answer := mixedAnswer(questions, answers)
		must(t, env.b.onItem(textCtx(env.tb, liveAuthorID, answer)), "onItem answer")
		prevScreen, priorRound = cs.Screen, cs.Round
	}

	if cs.Kind != "mixed" {
		t.Errorf("%s: kind=%q, ожидался mixed", name, cs.Kind)
	}
	for _, key := range []string{"case", "wrong", "need", "why"} {
		if cs.Filled[key] == "" {
			t.Errorf("%s: пункт ядра %q не закрыт: filled=%v", name, key, cs.Filled)
		}
	}
	assertHeadings(t, env.cases, caseID)

	cs = publish(t, env, caseID, cs.Screen, closeAfter)

	issue := fetchIssue(t, env, cs)
	labels := issue.LabelNames()
	if !slices.Contains(labels, "type:bug") || !slices.Contains(labels, "type:feature") {
		t.Errorf("%s: метки issue: %v, ожидались обе type:bug и type:feature", name, labels)
	}
	assertGapConsistency(t, cs, issue)

	t.Logf("%s: ГЕЙТ B - issue=#%d %s kind=%s gaps=%v labels=%v events(%s) failed=%t",
		name, cs.IssueNumber, issue.HTMLURL, cs.Kind, cs.Gaps, labels, eventCounts(t, env.cases, caseID), t.Failed())
}

// runSkipScenario - S5, R5 пропуск: раунд 1 обязателен, «Отправить как есть»
// закрывает его без ответа, повтор той же кнопки на устаревшем экране даёт
// «Этот экран устарел», а правка текстом после саммари пересобирает саммари
// без нового раунда (Р-15).
func runSkipScenario(t *testing.T, env *liveEnv) {
	caseID := startCase(t, env)
	cs := sendMaterial(t, env, caseID, []string{"Напоминания опять приходят неправильно"})
	prevScreen := cs.Screen
	finishCollect(t, env)

	cs, roundOccurred := waitForRoundOrSummary(t, env, caseID, 0, prevScreen)
	if !roundOccurred {
		t.Fatal("ожидался раунд 1, саммари пришло без вопросов")
	}
	roundScreen, round := cs.Screen, cs.Round

	pressSkip(t, env, roundScreen, round, "Принято")

	cs = waitForCase(t, env, caseID, roundScreen, func(cs *Case) bool { return cs.Status == statusSummary })
	if n := countEventKind(t, env.cases, caseID, "questions_skipped"); n != 1 {
		t.Errorf("событий пропуска: %d, ожидалась 1", n)
	}
	summaryScreen := cs.Screen

	// Повтор той же кнопки на уже неживом (раундовом) экране - устарел.
	pressSkip(t, env, roundScreen, round, "Этот экран устарел")
	if n := countEventKind(t, env.cases, caseID, "questions_skipped"); n != 1 {
		t.Errorf("повторный пропуск завёл второе событие: событий %d, ожидалась 1", n)
	}

	must(t, env.b.onItem(textCtx(env.tb, liveAuthorID, "Речь про напоминание о встрече за день")), "onItem fix")
	cs = waitForCase(t, env, caseID, summaryScreen, func(cs *Case) bool { return cs.Status == statusSummary })
	if n := countEventKind(t, env.cases, caseID, "round_asked"); n != 1 {
		t.Errorf("после правки после пропуска открылся новый раунд: событий round_asked %d, ожидалась 1", n)
	}
	if !cs.Incomplete || len(cs.Gaps) == 0 {
		t.Errorf("исход: incomplete=%t gaps=%v, ожидались непустые (ядро так и не закрыто)", cs.Incomplete, cs.Gaps)
	}
	assertHeadings(t, env.cases, caseID)

	cs = publish(t, env, caseID, cs.Screen, true)

	issue := fetchIssue(t, env, cs)
	assertGapConsistency(t, cs, issue)
	if !slices.Contains(issue.LabelNames(), "incomplete") {
		t.Errorf("метки issue: %v, ожидалась incomplete", issue.LabelNames())
	}

	t.Logf("S5: issue=#%d kind=%s gaps=%v labels=%v events(%s) failed=%t",
		cs.IssueNumber, cs.Kind, cs.Gaps, issue.LabelNames(), eventCounts(t, env.cases, caseID), t.Failed())
}

func mustQuestions(t *testing.T, cases *Cases, caseID string) []Question {
	t.Helper()
	questions, err := cases.lastQuestions(context.Background(), caseID)
	if err != nil {
		t.Fatalf("read last round questions of case %s: %v", caseID, err)
	}
	return questions
}

// questionKeys - ключи вопросов раунда для лога: разбор живого прогона хочет
// видеть, что именно спросила модель, а сценарии саму формулировку ключа не
// проверяют (недетерминировано).
func questionKeys(questions []Question) []string {
	keys := make([]string, len(questions))
	for i, q := range questions {
		keys[i] = q.Key
	}
	return keys
}
