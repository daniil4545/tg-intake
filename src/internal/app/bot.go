package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	tele "gopkg.in/telebot.v4"
)

const (
	dbTimeout = 10 * time.Second
	// Скачивание вложения идёт мимо контекста: telebot.Download его не
	// принимает. Бюджет обработчика сырья считаем по 20 МБ файла, иначе
	// deadline срабатывает уже после скачивания, на записи пути в БД.
	collectTimeout = 2 * time.Minute
	// Попыток поднять бота. Егресс к Telegram с этого VPS нестабилен, и один
	// неудачный getMe не должен ронять процесс; пятый подряд остаётся падением.
	startAttempts = 5
	// Бюджет запроса к Bot API при работе через прокси: больше цикла опроса и с
	// запасом на скачивание вложения.
	botTimeout = 2 * time.Minute
	// Потолок Bot API - 4096 символов. Протокол с разбором скриншотов его
	// перебирает, а отказ Telegram отправил бы работу notify в повторы, и автор
	// не увидел бы результат вовсе. Считается рунами, а не байтами: русский текст
	// весит по два байта на символ, и байтовый предел резал сообщения вдвое
	// раньше, чем нужно.
	maxMessage = 4000
	// Пачка пересылок без активного обращения приходит за секунды; меню в
	// ответ на неё должно быть одно, а не по числу сообщений.
	menuRepeat = 10 * time.Second
	// Через сколько молчания бот говорит автору, что ещё работает. Ход модели
	// обычно занимает секунды, но провайдер иногда отвечает дольше, и тишина
	// читается как «бот завис» - именно с этим приходят жалобы.
	waitNotice = time.Minute
	// Сколько живёт обещание «пришлю ссылку» после «Добавить проект».
	linkWait = 10 * time.Minute
	// Сколько getUpdates держит соединение, пауза после отказа опроса и шаг
	// записи отметки живости.
	pollTimeout = 10 * time.Second
	pollRetry   = 5 * time.Second
	aliveTick   = 30 * time.Second
	// Возраст отметки, после которого контейнер считается нездоровым. С запасом
	// над циклом опроса: одиночный отказ Bot API не повод для перезапуска.
	aliveTTL = 3 * time.Minute
)

// Инлайн-кнопки: telebot маршрутизирует callback по Unique и кодирует кнопку
// как \f<unique>|<data>. Свой формат callback_data мимо Unique хендлером не
// поймается. Кнопки сбора - reply-клавиатура, они маршрутизируются по тексту,
// поэтому один и тот же Btn идёт и в Handle, и в клавиатуру.
var (
	projectBtn  = &tele.Btn{Unique: "project"}
	createBtn   = &tele.Btn{Unique: "create"}
	askBtn      = &tele.Btn{Unique: "ask"}
	toTicketBtn = &tele.Btn{Unique: "to_ticket"}
	endAskBtn   = &tele.Btn{Unique: "end_ask"}
	continueBtn = &tele.Btn{Unique: "continue"}
	allTrueBtn  = &tele.Btn{Unique: "all_true"}
	// skipBtn - «Отправить как есть», data несёт номер раунда, как у allTrueBtn.
	skipBtn       = &tele.Btn{Unique: "skip"}
	publishBtn    = &tele.Btn{Unique: "publish"}
	fixBtn        = &tele.Btn{Unique: "fix"}
	ticketsBtn    = &tele.Btn{Unique: "tickets"}
	cardBtn       = &tele.Btn{Unique: "card"}
	killBtn       = &tele.Btn{Unique: "kill"}
	addProjectBtn = &tele.Btn{Unique: "add_project"}
	homeBtn       = &tele.Btn{Unique: "home"}
	// Возврат в меню проекта отдельной кнопкой: projectBtn - это выбор, и он
	// переставляет проект активного обращения. Навигация состояние не меняет.
	backBtn     = &tele.Btn{Unique: "back"}
	resetYesBtn = &tele.Btn{Unique: "reset_yes"}
	resetNoBtn  = &tele.Btn{Unique: "reset_no"}
	doneBtn     = &tele.Btn{Text: buttonDone}
	menuBtn     = &tele.Btn{Text: buttonMenu}
	resetBtn    = &tele.Btn{Text: buttonReset}
)

type Bot struct {
	bot      *tele.Bot
	pool     *pgxpool.Pool
	cases    *Cases
	tickets  *Tickets
	projects *Projects
	log      *slog.Logger
	allowed  []int64
	maxItems int
	// Отправитель уведомлений владельцу: бот сервиса либо отдельный бот канала
	// алертов. nil - уведомления выключены конфигом либо отправитель не собрался;
	// что именно, говорит лог старта.
	alert *tele.Bot
	// Когда автору в последний раз отвечали меню на свободное сообщение.
	// Хендлеры идут последовательно (Synchronous), мьютекс не нужен; потеря
	// при рестарте безвредна - автор получит меню лишний раз.
	menuAt map[int64]time.Time
	// Ожидание ссылки после «Добавить проект»: следующий текст автора - ссылка
	// на репозиторий. Та же дисциплина, что и menuAt: последовательные хендлеры,
	// потеря при рестарте безвредна - автор нажмёт кнопку снова.
	awaitLink map[int64]time.Time
	// Ход, о задержке которого автору уже сказали: ответов подряд бывает
	// несколько, а предупреждение нужно одно. Ход - это обращение вместе с
	// номером раунда: по одному номеру следующее обращение автора считалось бы
	// тем же ходом и осталось бы без предупреждения (раунд нового обращения
	// снова нулевой). Пишут поллер бота и воркер через Notify, а конкурентная
	// запись в map фатальна - только под mu.
	mu     sync.Mutex
	waited map[int64]string
}

func NewBot(ctx context.Context, cfg Config, pool *pgxpool.Pool, cases *Cases, tickets *Tickets, projects *Projects, log *slog.Logger) (*Bot, error) {
	b := &Bot{pool: pool, cases: cases, tickets: tickets, projects: projects, log: log,
		allowed: cfg.AllowedIDs, maxItems: cfg.MaxItems,
		menuAt: map[int64]time.Time{}, awaitLink: map[int64]time.Time{},
		waited: map[int64]string{}}

	// Verbose дампит сырые payload Bot API с текстами сообщений, а стандартный
	// OnError пишет через stdlib log мимо JSON. Synchronous обязателен:
	// параллельные хендлеры ломают одно обращение - раздел 5 architecture.md.
	tb, err := startBot(ctx, tele.Settings{
		Token:       cfg.BotToken,
		Poller:      &poller{log: log, alivePath: alivePath(cfg.MediaDir)},
		Client:      proxyClient(cfg.TelegramProxy),
		Verbose:     false,
		Synchronous: true,
		OnError: func(err error, c tele.Context) {
			log.Error("handler_failed", "error", err, "user_id", senderID(c))
			// Молчание на действие читается как зависший бот: автор обязан
			// получить хоть какой-то ответ. Best-effort: ошибку отправки уже
			// некому чинить.
			if c != nil {
				if c.Callback() != nil {
					_ = c.Respond()
				}
				_ = c.Send(msgHandlerFailed)
			}
		},
	}, log)
	if err != nil {
		return nil, err
	}

	tb.Use(b.allow)
	b.routes(tb)

	// Меню «/» в клиенте. Отказ не роняет старт: бот работает и без него.
	if err := tb.SetCommands([]tele.Command{
		{Text: "start", Description: commandMenu},
		{Text: "tickets", Description: commandTickets},
		{Text: "project", Description: commandAddProject},
		{Text: "cancel", Description: commandReset},
	}); err != nil {
		log.Warn("set_commands_failed", "error", err)
	}

	b.bot = tb

	// Отказ отправителя уведомлений не роняет старт: приём обращений не должен
	// зависеть от чужого бота - раздел 3 спеки.
	alert, err := newAlertBot(cfg, tb)
	if err != nil {
		log.Error("alert_bot_failed", "error", err)
	}
	b.alert = alert
	log.Info("alerts_enabled", "on", alert != nil, "own_bot", cfg.AlertBotToken != "")

	return b, nil
}

// routes регистрирует хендлеры бота на tb. Вынесена из NewBot, чтобы тест
// маршрутизации (TestUnknownButtonStale) мог собрать те же обработчики без
// сети, поллера и токена и прогнать апдейт через tb.ProcessUpdate - только
// так тест ловит потерю привязки хендлера, а не только поведение самой
// функции при прямом вызове.
func (b *Bot) routes(tb *tele.Bot) {
	tb.Handle("/start", b.onStart)
	tb.Handle("/done", b.onDone)
	tb.Handle("/cancel", b.onReset)
	tb.Handle("/tickets", b.onTicketList)
	tb.Handle("/project", b.onProjectAdd)

	tb.Handle(projectBtn, b.onProject)
	tb.Handle(ticketsBtn, b.onTickets)
	tb.Handle(cardBtn, b.onCard)
	tb.Handle(killBtn, b.onKill)
	tb.Handle(createBtn, b.onCreate)
	tb.Handle(askBtn, b.onAsk)
	tb.Handle(toTicketBtn, b.onToTicket)
	tb.Handle(endAskBtn, b.onEndAsk)
	tb.Handle(continueBtn, b.onContinue)
	tb.Handle(allTrueBtn, b.onAllTrue)
	tb.Handle(skipBtn, b.onSkip)
	tb.Handle(publishBtn, b.onPublish)
	tb.Handle(fixBtn, b.onFix)
	tb.Handle(addProjectBtn, b.onAddProject)
	tb.Handle(homeBtn, b.onHome)
	tb.Handle(backBtn, b.onProjectMenu)
	tb.Handle(resetYesBtn, b.onResetYes)
	tb.Handle(resetNoBtn, b.onResetNo)
	tb.Handle(doneBtn, b.onDone)
	tb.Handle(menuBtn, b.onMenu)
	tb.Handle(resetBtn, b.onReset)

	// Кнопки прежних версий («Отмена» под саммари, «Начать заново») хендлеров
	// больше не имеют, и незнакомый callback - тот же факт, что устаревшая
	// кнопка шага (правило 3 §2.1): без ответа нажатие выглядело бы вечным
	// спиннером.
	tb.Handle(tele.OnCallback, b.staleButton)

	// Правка уже отправленного сообщения: сырьё дописывается, а не
	// переписывается, поэтому учесть её нечем. Без хендлера апдейт не доходит
	// никуда, и автор считает, что бот прочитал исправленное.
	tb.Handle(tele.OnEdited, func(c tele.Context) error {
		return c.Send(msgEditNotSeen)
	})

	tb.Handle(tele.OnText, b.onItem)
	tb.Handle(tele.OnVoice, b.onItem)
	tb.Handle(tele.OnPhoto, b.onItem)
	tb.Handle(tele.OnDocument, b.onItem)
	tb.Handle(tele.OnVideo, b.onItem)
	// OnMedia обязателен фолбэком: не найдя ни специфичного хендлера, ни его,
	// telebot молча роняет апдейт, и аудиофайл, стикер, гифка или кружок
	// исчезают без единого слова - хуже отказа, потому что автор считает, что
	// отправил.
	tb.Handle(tele.OnMedia, b.onItem)
}

// proxyClient - клиент Bot API; пустой адрес прокси даёт nil, telebot возьмёт
// свой клиент с прямым путём. net/http сам понимает http и socks5 в адресе.
// Таймаут с запасом над long polling (держит соединение десять секунд):
// скачивание вложения через туннель бывает и дольше.
func proxyClient(proxy string) *http.Client {
	if proxy == "" {
		return nil
	}
	// Адрес разобран и проверен при загрузке конфига.
	parsed, _ := url.Parse(proxy)
	return &http.Client{
		Timeout:   botTimeout,
		Transport: &http.Transport{Proxy: http.ProxyURL(parsed)},
	}
}

// startBot повторяет getMe с отсрочкой: егресс к Telegram нестабилен даже с
// закреплённым DC, но пятый подряд отказ остаётся падением.
// poller - long polling со следом в логе и с отметкой живости. Свой вместо
// tele.LongPoller: тот отдаёт ошибку опроса в debug, который молчит без
// Verbose, а Verbose дампит сырые payload Bot API с текстами обращений.
// Из-за этого 409 Conflict от второго поллера с тем же токеном не виден ни в
// логе, ни снаружи, а цикл опроса при постоянной ошибке крутится без паузы.
type poller struct {
	log       *slog.Logger
	alivePath string
	lastID    int
	marked    time.Time
}

func (p *poller) Poll(b *tele.Bot, dest chan tele.Update, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}

		updates, err := p.fetch(b)
		if err != nil {
			p.log.Error("poll_failed", "error", err)
			// Пауза обязательна: отказ возвращается сразу, и цикл без неё жарит
			// Bot API отказами.
			if !sleepOrStop(stop, pollRetry) {
				return
			}
			continue
		}
		p.mark()

		for _, update := range updates {
			p.lastID = update.ID
			select {
			case dest <- update:
			case <-stop:
				return
			}
		}
	}
}

// fetch разбирает ответ сам: Raw отдаёт тело как есть, и отказ Bot API
// (`ok: false`) без этой проверки выглядел бы пустой пачкой апдейтов - ровно
// тот случай, ради которого поллер и переписан.
func (p *poller) fetch(b *tele.Bot) ([]tele.Update, error) {
	data, err := b.Raw("getUpdates", map[string]any{
		"offset":  p.lastID + 1,
		"timeout": int(pollTimeout.Seconds()),
	})
	if err != nil {
		return nil, err
	}

	var resp struct {
		OK          bool          `json:"ok"`
		Result      []tele.Update `json:"result"`
		ErrorCode   int           `json:"error_code"`
		Description string        `json:"description"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode updates: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("getUpdates: %d %s", resp.ErrorCode, resp.Description)
	}
	return resp.Result, nil
}

// mark обновляет отметку живости. Не чаще одного раза в aliveTick: опрос
// возвращается каждые десять секунд, и запись на диск на каждый круг не нужна.
func (p *poller) mark() {
	if time.Since(p.marked) < aliveTick {
		return
	}
	now := time.Now()
	if err := os.WriteFile(p.alivePath, []byte(now.Format(time.RFC3339)), 0o600); err != nil {
		p.log.Error("alive_mark_failed", "error", err)
		return
	}
	p.marked = now
}

// PollerAlive - идёт ли опрос Telegram. Отметку обновляет успешный getUpdates,
// и её возраст - единственный признак, отличающий работающий поллер от
// молчащего: healthcheck запускается отдельным процессом и памяти сервиса не
// видит.
func PollerAlive(mediaDir string) bool {
	info, err := os.Stat(alivePath(mediaDir))
	return err == nil && time.Since(info.ModTime()) < aliveTTL
}

func alivePath(mediaDir string) string { return filepath.Join(mediaDir, "poller.alive") }

// sleepOrStop возвращает false, если бот остановлен и ждать больше незачем.
func sleepOrStop(stop chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-stop:
		return false
	case <-timer.C:
		return true
	}
}

func startBot(ctx context.Context, settings tele.Settings, log *slog.Logger) (*tele.Bot, error) {
	var err error
	for attempt := range startAttempts {
		var tb *tele.Bot
		tb, err = tele.NewBot(settings)
		if err == nil {
			return tb, nil
		}
		if attempt == startAttempts-1 {
			break
		}
		delay := time.Duration(1<<attempt) * time.Second
		log.Warn("bot_start_retry", "attempt", attempt+1, "delay", delay.String(), "error", err)
		if !wait(ctx, delay) {
			return nil, fmt.Errorf("create bot: %w", ctx.Err())
		}
	}
	return nil, fmt.Errorf("create bot after %d attempts: %w", startAttempts, err)
}

func (b *Bot) Start() { b.bot.Start() }

func (b *Bot) Stop() { b.bot.Stop() }

// Notify доставляет сообщение, порождённое фоновой работой: протокол после
// нормализации и провал цепочки. Регистрируется в main.go обработчиком работы
// notify. Синхронно из хендлера уходят только ответы внутри диалога, результат
// фоновой работы обязан пережить рестарт и потому идёт очередью.
func (b *Bot) Notify(ctx context.Context, job Job) error {
	var p notifyPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return fmt.Errorf("unmarshal notify payload: %w", err)
	}
	if p.Text == "" {
		return fmt.Errorf("notify of case %s has no text", p.CaseID)
	}

	// Уведомление владельцу: адресат назван в работе, обращение для него не
	// нужно. Выключенные уведомления гасят уже поставленную работу молча -
	// иначе она крутилась бы в повторах до провала.
	if p.ChatID != 0 {
		if b.alert == nil {
			b.log.Warn("alert_skipped", "case_id", p.CaseID, "chat_id", p.ChatID)
			return nil
		}
		if _, err := b.alert.Send(&tele.Chat{ID: p.ChatID}, p.Text); err != nil {
			return fmt.Errorf("send alert: %w", err)
		}
		return nil
	}

	cs, err := b.cases.Load(ctx, p.CaseID)
	if err != nil {
		return err
	}
	if cs == nil {
		return fmt.Errorf("notify: case %s not found", p.CaseID)
	}

	// Исход отмены тикета правит тот экран, с которого её запустили: автор
	// смотрит на «Отменяю тикет #N», и ответ обязан прийти туда же. Экран
	// живёт в cases.screen_msg (Р-8), а не в памяти процесса, поставившего
	// работу - переживает рестарт и повторную доставку.
	if p.Buttons == keysCancel {
		return b.finishKill(ctx, cs, p.Text)
	}

	// Новость по тикету к жизни разговора отношения не имеет: автор мог в этот
	// момент собирать новое обращение, и трогать его живой экран нельзя.
	if p.Buttons == keysTicket {
		return b.sendNews(ctx, cs, p.Text)
	}

	// Переход снимает живой экран раньше своей отправки: устаревшую кнопку не
	// должно быть кому ловить правилом 3. Экран раунда сюда не входит: «to-
	// ticket» кладёт keysHome в одной транзакции со сменой режима, и, если этот
	// notify доставлен с опозданием, за это время уже мог появиться свой живой
	// экран следующего раунда - закрывать чужой прогресс нельзя. Экран саммари
	// и счётчика сбора всегда round == 0, их закрытие такому риску не подвержено.
	if p.Buttons == keysHome {
		if cs.ScreenRound == 0 {
			b.closeScreen(ctx, cs)
		}
		_, err := b.sendLong(&tele.User{ID: cs.UserID}, p.Text, homeKeyboard())
		return err
	}

	// Раунд вопросов и саммари - шаг: прежний живой экран снимается, новый
	// текст становится следующим. Запоздавшая доставка - раунд, который автор
	// уже прошёл (payload.Round < cs.Round), или саммари, которое больше не
	// текущее (status != summary) - шагом не становится: обычное сообщение без
	// кнопок шага, живой экран не трогает.
	switch p.Buttons {
	case keysRound, keysAsk:
		if cs.Status == statusInterview && p.Round >= cs.Round {
			return b.showStep(ctx, cs, cs.Round, p.Text, roundKeyboard(cs.Round, p.Buttons == keysRound))
		}
		_, err := b.sendLong(&tele.User{ID: cs.UserID}, p.Text)
		return err
	case keysSummary:
		if cs.Status == statusSummary {
			return b.showStep(ctx, cs, 0, p.Text, summaryKeyboard())
		}
		_, err := b.sendLong(&tele.User{ID: cs.UserID}, p.Text)
		return err
	}

	var opts []any
	switch {
	// Ответ из документации не шаг: кнопки решения важнее панели сбора, она в
	// режиме вопроса и так на месте.
	case p.Buttons == keysAnswer:
		opts = append(opts, answerKeyboard())
	// Провал нормализации возвращает обращение в сбор, а кнопки сбора сняты
	// нажатием «Готово»: без них автор не поймёт, чем закончить второй заход.
	case cs.Status == statusCollecting:
		opts = append(opts, collectKeyboard())
	}
	if _, err := b.sendLong(&tele.User{ID: cs.UserID}, p.Text, opts...); err != nil {
		return err
	}
	// Ответ показан: прежний живой экран (счётчик или раунд) больше не в счёт,
	// но кнопки его не наши - снимать нечего, только забыть в базе. Автор уже
	// получил ответ - отказ записи не идёт наверх, иначе повтор работы прислал
	// бы тот же ответ вторым сообщением.
	if p.Buttons == keysAnswer {
		if err := b.cases.ResetScreen(ctx, cs.ID, cs.Screen); err != nil {
			b.log.Warn("screen_reset_failed", "case_id", cs.ID, "error", err)
		}
	}
	return nil
}

// sendNews доставляет новость по тикету: одна кнопка перехода в карточку.
// Сообщение новое, а не правка экрана: автор мог смотреть куда угодно, а новость
// обязана попасть в переписку.
func (b *Bot) sendNews(ctx context.Context, cs *Case, text string) error {
	if cs.ProjectID == nil || cs.IssueNumber == 0 {
		return fmt.Errorf("news of case %s has no ticket", cs.ID)
	}
	project, err := LoadProject(ctx, b.pool, *cs.ProjectID)
	if err != nil {
		return err
	}
	markup := &tele.ReplyMarkup{}
	markup.Inline(markup.Row(markup.Data(buttonOpenTicket(cs.IssueNumber),
		cardBtn.Unique, cardData(project.Slug, cs.IssueNumber))))
	_, err = b.sendLong(&tele.User{ID: cs.UserID}, text, markup)
	return err
}

// finishKill доставляет исход отмены тикета: правит карточку, с которой была
// нажата «Отменить тикет» (cases.screen_msg, записан в Tickets.Cancel, Р-8) -
// переживает рестарт и повторную доставку, в отличие от памяти процесса.
// Исход всегда ведёт на первую страницу списка (решение 6 плана среза):
// карточка не хранит, откуда пришли к ней, дольше своего сообщения.
func (b *Bot) finishKill(ctx context.Context, cs *Case, text string) error {
	project, err := b.projectOf(ctx, cs)
	if err != nil {
		return err
	}
	if project == nil {
		return fmt.Errorf("finish kill of case %s: project is unknown", cs.ID)
	}
	markup := backToList(project.Slug, 0)

	if cs.Screen == 0 {
		// Отмена состоялась до записи экрана (срез до 0012) либо экран потерян:
		// автор обязан узнать исход, даже без карточки для правки.
		b.log.Info("kill_screen_missing", "case_id", cs.ID)
		_, err := b.sendLong(&tele.User{ID: cs.UserID}, text, markup)
		return err
	}

	msg := tele.StoredMessage{MessageID: strconv.Itoa(cs.Screen), ChatID: cs.UserID}
	sent, err := b.editScreen(&tele.User{ID: cs.UserID}, msg, text, markup)
	if err != nil {
		return err
	}
	if sent != nil {
		// Правка не удалась (M2, как у onFix): screen_msg переезжает на новое
		// сообщение - иначе повтор notify или следующая карточка правили бы
		// сообщение, которого автор уже не видит рабочим.
		return b.cases.SetScreen(ctx, cs.ID, sent.ID, 0)
	}
	return nil
}

// sendLong режет длинный текст по границе строки: протокол не влезает в одно
// сообщение, а отказ Telegram увёл бы работу в повторы. Возвращает последний
// кусок - тот, что несёт кнопки.
func (b *Bot) sendLong(to tele.Recipient, text string, opts ...any) (*tele.Message, error) {
	for {
		if utf8.RuneCountInString(text) <= maxMessage {
			sent, err := b.bot.Send(to, text, opts...)
			if err != nil {
				return nil, fmt.Errorf("send message: %w", err)
			}
			return sent, nil
		}

		// Голова - ровно maxMessage рун; её длина в байтах и есть предел резки,
		// потому что голова - префикс текста.
		head := cutRunes(text, maxMessage)
		cut := strings.LastIndex(head, "\n")
		if cut <= 0 {
			cut = len(head)
		}
		// opts только на последнем куске: иначе кнопки дублируются под каждым, и
		// автор жмёт устаревшую копию выше по переписке.
		if _, err := b.bot.Send(to, strings.TrimRight(text[:cut], "\n")); err != nil {
			return nil, fmt.Errorf("send message part: %w", err)
		}
		text = strings.TrimLeft(text[cut:], "\n")
	}
}

// showStep - шаг разговора: снять кнопки прежнего живого экрана, отправить
// текст следующего и запомнить его вместе с раундом. Кнопки нового сообщения
// (если есть) - на последнем куске sendLong, он и есть новый экран. Гонка со
// снятием параллельного шага не страшна: устаревшую копию ловит liveScreen.
func (b *Bot) showStep(ctx context.Context, cs *Case, round int, text string, opts ...any) error {
	b.stripScreen(cs, cs.Screen)

	sent, err := b.sendLong(&tele.User{ID: cs.UserID}, text, opts...)
	if err != nil {
		return err
	}
	if err := b.cases.SetScreen(ctx, cs.ID, sent.ID, round); err != nil {
		return err
	}
	cs.Screen, cs.ScreenRound = sent.ID, round
	return nil
}

// closeScreen снимает кнопки живого экрана на переходе и обнуляет его в базе:
// следующий шаг заводит новый экран с нуля. Экрана нет - снимать нечего.
func (b *Bot) closeScreen(ctx context.Context, cs *Case) {
	if cs.Screen == 0 {
		return
	}
	b.stripScreen(cs, cs.Screen)
	if err := b.cases.ResetScreen(ctx, cs.ID, cs.Screen); err != nil {
		b.log.Warn("screen_reset_failed", "case_id", cs.ID, "error", err)
		return
	}
	cs.Screen, cs.ScreenRound = 0, 0
}

// stripScreen снимает инлайн-кнопки чужого или прежнего экрана. id 0 - экрана
// не было. Отказ Telegram шаг не останавливает: устаревшую кнопку в худшем
// случае поймает liveScreen, а лог даёт знать про контур, где сообщение нельзя
// отредактировать (раздел 9 плана среза).
func (b *Bot) stripScreen(cs *Case, msgID int) {
	if msgID == 0 {
		return
	}
	msg := tele.StoredMessage{MessageID: strconv.Itoa(msgID), ChatID: cs.UserID}
	if err := b.stripButtons(msg); err != nil {
		b.log.Warn("screen_strip_failed", "case_id", cs.ID, "message_id", msgID, "error", err)
	}
}

// stripButtons снимает инлайн-кнопки сообщения. «Уже не изменено» - успех:
// кнопок и так нет, второй вызов (гонка с параллельным шагом) не отказ.
func (b *Bot) stripButtons(msg tele.Editable) error {
	_, err := b.bot.EditReplyMarkup(msg, nil)
	if err == nil || errors.Is(err, tele.ErrMessageNotModified) || errors.Is(err, tele.ErrSameMessageContent) {
		return nil
	}
	return err
}

// liveScreen - правило 3 §2.1: нажатая кнопка шага действует, только если она
// с живого экрана обращения. Экрана нет (screen_msg == 0 - счётчик сбора
// кнопок не несёт, переход его уже снял) - сверять нечего. Кнопка с чужого
// сообщения - устаревший шаг: единственный ответ на неё - staleButton.
func (b *Bot) liveScreen(c tele.Context, cs *Case) bool {
	if cs.Screen == 0 {
		return true
	}
	msg := c.Message()
	if msg != nil && msg.ID == cs.Screen {
		return true
	}
	_ = b.staleButton(c)
	return false
}

// staleButton - единственный ответ на устаревшую, незнакомую или битую
// кнопку (правило 3 §2.1): toast «Этот экран устарел» и снятие кнопок
// нажатого сообщения, больше ничего. Второй ответ на тот же callback
// Telegram отклоняет, поэтому вызывающий не должен отвечать на него ни до,
// ни после - это всегда последний шаг обработчика.
func (b *Bot) staleButton(c tele.Context) error {
	b.toast(c, toastStale)
	if msg := c.Message(); msg != nil {
		if err := b.stripButtons(msg); err != nil {
			b.log.Warn("screen_strip_failed", "user_id", senderID(c), "error", err)
		}
	}
	return nil
}

// allow - единственная точка контроля доступа: обойти её хендлером нельзя.
func (b *Bot) allow(next tele.HandlerFunc) tele.HandlerFunc {
	return func(c tele.Context) error {
		sender := c.Sender()
		if sender == nil {
			b.log.Warn("update_without_sender")
			return nil
		}
		if chat := c.Chat(); chat == nil || chat.Type != tele.ChatPrivate {
			// Скриншоты и саммари обращения групповому чату не место. Лог
			// обязателен: без него этот отказ - единственный, которого не видно
			// в контуре, и разбор жалобы «бот меня не пустил» упирается в пустоту.
			b.log.Warn("access_denied", "user_id", sender.ID, "reason", "not_private")
			return refuse(c, msgPrivateOnly)
		}
		if !slices.Contains(b.allowed, sender.ID) {
			b.log.Warn("access_denied", "user_id", sender.ID)
			return refuse(c, msgAccessDenied)
		}
		// Ожидание ссылки после «Добавить проект» разбирается до маршрутизации.
		// Команда, кнопка и слово панели его снимают: скрытый режим не должен
		// пережить смену темы, а нажатие «Меню» не должно разбираться как ссылка.
		if since, ok := b.awaitLink[sender.ID]; ok {
			delete(b.awaitLink, sender.ID)
			if c.Callback() == nil && !isCommand(c) && !panelWord(strings.TrimSpace(c.Text())) &&
				time.Since(since) < linkWait {
				return b.onProjectLink(c)
			}
		}
		return next(c)
	}
}

// panelWord - тексты reply-кнопок: они маршрутизируются по точному совпадению
// и не могут быть ни ссылкой, ни сырьём вне сбора.
func panelWord(text string) bool {
	return text == doneBtn.Text || text == menuBtn.Text || text == resetBtn.Text
}

// toast - мгновенный отклик на нажатие: подсказка Telegram гасит спиннер
// кнопки до похода в базу, GitHub или к модели. Отказ подсказки действие не
// отменяет: у протухшего callback нажатие живое, терять публикацию нельзя.
func (b *Bot) toast(c tele.Context, text string) {
	if err := c.Respond(&tele.CallbackResponse{Text: text}); err != nil {
		b.log.Warn("toast_failed", "user_id", senderID(c), "error", err)
	}
}

// screen переписывает сообщение, на кнопку которого нажали: экран меняется на
// месте, отработавшие кнопки исчезают, история не растёт копиями меню. Тонкая
// обёртка над editScreen (правило 2 §2.1): отправленное сообщение запасного
// пути тут не нужно никому, кроме onFix, который зовёт editScreen сам.
func (b *Bot) screen(c tele.Context, text string, markup *tele.ReplyMarkup) error {
	_, err := b.editScreen(c.Recipient(), c.Message(), text, markup)
	return err
}

// editScreen - единственный путь правки навигации и запасного пути шага
// (правило 2 §2.1). Текст длиннее предела Telegram или экрана для правки нет
// вовсе (msg == nil) - в правку не помещается, экран уходит sendLong с
// прежнего сообщения снимаются кнопки. Иначе - правка; «то же содержимое» и
// «уже не изменено» - успех без повторной отправки, прочий отказ - лог и тот
// же запасной путь.
//
// Возврат: nil при успешной правке (сообщение осталось тем же), иначе -
// отправленное запасным путём сообщение (onFix переносит на него screen_msg,
// M2). Ошибка отправки уходит наверх, отказ снятия кнопок старого экрана -
// только в лог.
func (b *Bot) editScreen(to tele.Recipient, msg tele.Editable, text string, markup *tele.ReplyMarkup) (*tele.Message, error) {
	var opts []any
	if markup != nil {
		opts = append(opts, markup)
	}

	if msg != nil && utf8.RuneCountInString(text) <= maxMessage {
		_, err := b.bot.Edit(msg, text, opts...)
		if err == nil || errors.Is(err, tele.ErrSameMessageContent) || errors.Is(err, tele.ErrMessageNotModified) {
			return nil, nil
		}
		msgID, chatID := msg.MessageSig()
		b.log.Warn("screen_edit_failed", "chat_id", chatID, "message_id", msgID, "error", err)
	}

	sent, err := b.sendLong(to, text, opts...)
	if err != nil {
		return nil, err
	}
	if msg != nil {
		if err := b.stripButtons(msg); err != nil {
			msgID, chatID := msg.MessageSig()
			b.log.Warn("screen_strip_failed", "chat_id", chatID, "message_id", msgID, "error", err)
		}
	}
	return sent, nil
}

// sendPanel - сообщение-переход: единственный способ сменить нижнюю панель
// (правило 4 §2.1). Инлайн-кнопок не несёт по определению - у сообщения одно
// поле клавиатуры, а панель и экран делят его.
func (b *Bot) sendPanel(c tele.Context, text string) error {
	return c.Send(text, homeKeyboard())
}

// refuse отвечает отказом и гасит спиннер, если отказ пришёл на нажатие
// кнопки: без Respond кнопка крутится до таймаута Telegram.
func refuse(c tele.Context, text string) error {
	if c.Callback() != nil {
		if err := c.Respond(); err != nil {
			return err
		}
	}
	return c.Send(text)
}

func senderID(c tele.Context) int64 {
	if c == nil || c.Sender() == nil {
		return 0
	}
	return c.Sender().ID
}

// author собирает профиль автора из апдейта. ФИО перечитывается при каждом
// обращении: люди меняют его в профиле, а шапка issue берёт актуальное.
func author(c tele.Context) User {
	s := c.Sender()
	return User{ID: s.ID, First: s.FirstName, Last: s.LastName, Username: s.Username}
}

func (b *Bot) onStart(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	if err := UpsertUser(ctx, b.pool, author(c)); err != nil {
		return err
	}

	// Активное обращение занимает единственный слот автора, и начальный экран
	// из него ведёт в тупик; вдобавок он сменил бы панель сбора на «Меню |
	// Сброс», и «Готово» пропала бы с экрана. Говорим прямо, где автор стоит.
	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs != nil {
		return b.sendState(c, cs)
	}

	b.log.Info("start", "user_id", senderID(c))
	return b.homeScreen(ctx, c, msgBotIntro)
}

// homeScreen - домашняя навигация: список проектов, одним сообщением (правило
// 4 §2.1 - нижняя панель отдельного сообщения ради себя не получает). intro
// пустой на прямом входе после перехода: вступление уже сказал sendPanel.
func (b *Bot) homeScreen(ctx context.Context, c tele.Context, intro string) error {
	markup, err := b.projectsMarkup(ctx)
	if err != nil {
		return err
	}
	text := homeText
	if intro != "" {
		text = intro + "\n\n" + homeText
	}
	return c.Send(text, markup)
}

// onHome - «К проектам» с любого экрана. Возврат правкой того же сообщения:
// экран без пути назад автор читает как тупик и идёт искать команду.
func (b *Bot) onHome(c tele.Context) error {
	b.toast(c, toastOpeningProjects)

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	markup, err := b.projectsMarkup(ctx)
	if err != nil {
		return err
	}
	return b.screen(c, homeText, markup)
}

func (b *Bot) projectsMarkup(ctx context.Context) (*tele.ReplyMarkup, error) {
	projects, err := ListProjects(ctx, b.pool)
	if err != nil {
		return nil, err
	}
	markup := &tele.ReplyMarkup{}
	rows := make([]tele.Row, 0, len(projects)+1)
	for _, p := range projects {
		rows = append(rows, markup.Row(markup.Data(p.Title, projectBtn.Unique, p.Slug)))
	}
	rows = append(rows, markup.Row(markup.Data(buttonAddProject, addProjectBtn.Unique)))
	markup.Inline(rows...)
	return markup, nil
}

// backHome и backProject - две ступени возврата: к списку проектов и к меню
// проекта. Дальше вглубь возврат уже есть у карточки тикета.
func backHome() *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	markup.Inline(markup.Row(markup.Data(buttonToProjects, homeBtn.Unique)))
	return markup
}

func backProject(slug string) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	markup.Inline(markup.Row(markup.Data(buttonProjectMenu, backBtn.Unique, slug)))
	return markup
}

// projectMenu - что можно делать с проектом. Один экран для выбора из списка и
// для возврата из тикетов.
func projectMenu(slug string) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	markup.Inline(
		markup.Row(markup.Data(buttonCreateTicket, createBtn.Unique, slug)),
		markup.Row(markup.Data(buttonAsk, askBtn.Unique, slug)),
		markup.Row(markup.Data(buttonViewTickets, ticketsBtn.Unique, slug)),
		markup.Row(markup.Data(buttonToProjects, homeBtn.Unique)),
	)
	return markup
}

// onProjectMenu - «Назад» со списка тикетов. Проект активного обращения не
// трогает: возврат по экранам не должен переставлять цель сбора.
func (b *Bot) onProjectMenu(c tele.Context) error {
	b.toast(c, toastOpeningProject)

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	project, ok, err := b.project(ctx, c.Data())
	if err != nil {
		return err
	}
	if !ok {
		return b.screen(c, msgProjectDisabled, backHome())
	}
	return b.screen(c, msgProjectOpened(project.Title), projectMenu(project.Slug))
}

// askProject показывает меню проектов. Вопрос задаётся ровно один раз за
// обращение (признак приходит из CollectItem) и повторяется на «Готово» без
// проекта: тикет некуда заводить.
func (b *Bot) askProject(ctx context.Context, c tele.Context, text string) error {
	return b.askProjectFor(ctx, c, text, projectBtn)
}

// askProjectFor - тот же список проектов под другую кнопку: выбор проекта нужен
// и сбору обращения, и просмотру тикетов.
func (b *Bot) askProjectFor(ctx context.Context, c tele.Context, text string, btn *tele.Btn) error {
	projects, err := ListProjects(ctx, b.pool)
	if err != nil {
		return err
	}
	if len(projects) == 0 {
		b.log.Warn("no_projects", "user_id", senderID(c))
		markup := &tele.ReplyMarkup{}
		markup.Inline(markup.Row(markup.Data(buttonAddProject, addProjectBtn.Unique)))
		return c.Send(msgNoProjectsYet, markup)
	}

	markup := &tele.ReplyMarkup{}
	rows := make([]tele.Row, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, markup.Row(markup.Data(p.Title, btn.Unique, p.Slug)))
	}
	markup.Inline(rows...)
	return c.Send(text, markup)
}

// onProject - выбор проекта из меню. Один хендлер на два входа: меню после
// /start и вопрос, заданный на первом элементе обращения. Разводит их
// состояние в БД, а не то, какой экран автор видел последним.
func (b *Bot) onProject(c tele.Context) error {
	// Первым делом: иначе у автора висит спиннер на кнопке.
	b.toast(c, toastOpeningProject)

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	projects, err := ListProjects(ctx, b.pool)
	if err != nil {
		return err
	}

	slug := c.Data()
	index := slices.IndexFunc(projects, func(p Project) bool { return p.Slug == slug })
	if index < 0 {
		b.log.Warn("unknown_project", "user_id", senderID(c), "slug", slug)
		return b.screen(c, msgProjectDisabled, backHome())
	}
	title := projects[index].Title

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	b.log.Info("project_selected", "user_id", senderID(c), "slug", slug)

	if cs == nil {
		// Экран проектов превращается в экран проекта: то же сообщение, другие
		// кнопки. Автор видит, что нажатие сработало, а список не остаётся
		// висеть кликабельным выше по переписке.
		return b.screen(c, msgProjectOpened(title), projectMenu(slug))
	}

	if err := b.cases.SetProject(ctx, cs, slug); err != nil {
		if errors.Is(err, ErrNotCollecting) {
			return b.sendState(c, cs)
		}
		if errors.Is(err, ErrUnknownProject) {
			return b.screen(c, msgProjectDisabled, backHome())
		}
		return err
	}
	if err := b.screen(c, msgProjectChosen(title), nil); err != nil {
		return err
	}
	// Панель сбора - reply-клавиатура, в правку сообщения она не помещается.
	return c.Send(msgSendMaterial, collectKeyboard())
}

// onCreate - «Создать тикет», onAsk - «Спросить». Вход один и тот же сбор
// материала, разный только режим разговора: он выбирает, чем кончится
// нормализация - интервью или походом в документацию.
func (b *Bot) onCreate(c tele.Context) error { return b.startCase(c, modeTicket) }

func (b *Bot) onAsk(c tele.Context) error { return b.startCase(c, modeAsk) }

// startCase заводит обращение выбранного режима. При живом обращении автор
// выбирает: продолжить его или начать заново. Активное обращение у автора одно,
// и молча подменять его нельзя - в нём уже лежит сырьё.
func (b *Bot) startCase(c tele.Context, mode string) error {
	if mode == modeAsk {
		b.toast(c, toastListening)
	} else {
		b.toast(c, toastCollecting)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	slug := c.Data()
	cs, existed, err := b.cases.StartCase(ctx, author(c), slug, mode)
	if err != nil {
		return err
	}
	if existed {
		// Выбор «продолжить или заново» уместен только в сборе: из разбора
		// продолжать нечего, там обращение двигает ответ автора. «Начать
		// заново» - это «Сброс» на панели плюс «Создать тикет», своей кнопки
		// у него нет.
		if cs.Status != statusCollecting {
			return b.sendState(c, cs)
		}
		markup := &tele.ReplyMarkup{}
		markup.Inline(markup.Row(markup.Data(buttonContinue, continueBtn.Unique, slug)))
		return b.screen(c, continueText(cs.Mode, mode), markup)
	}
	if mode == modeAsk {
		if err := b.screen(c, msgAnsweringDocs, nil); err != nil {
			return err
		}
		return c.Send(msgAskWhatToKnow, collectKeyboard())
	}
	if err := b.screen(c, msgCollectingCase, nil); err != nil {
		return err
	}
	return c.Send(msgSendMaterialCollect, collectKeyboard())
}

func (b *Bot) onContinue(c tele.Context) error {
	b.toast(c, toastContinuing)

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		return b.screen(c, msgCaseClosed, backHome())
	}
	if cs.Status != statusCollecting {
		return b.sendState(c, cs)
	}

	// Автор пришёл из меню конкретного проекта, значит продолжать надо в нём:
	// проект можно менять до «Готово», источник истины - строка в cases.
	// Кнопка живёт в чужом сообщении дольше экрана, поэтому смена проекта
	// молча запрещена: обращение уехало бы в чужой репозиторий, и автор узнал
	// бы об этом из готового тикета.
	if slug := c.Data(); slug != "" {
		switch known, err := b.projectOf(ctx, cs); {
		case err != nil:
			return err
		case known != nil && known.Slug != slug:
			return b.screen(c, msgContinueOtherProject(known.Title), nil)
		case known == nil:
			if err := b.cases.SetProject(ctx, cs, slug); err != nil && !errors.Is(err, ErrUnknownProject) {
				return err
			}
		}
	}
	// Продолжаем с чистого листа только теперь, когда переход точно состоится:
	// счётчик материала прежнего захода больше не в счёт, следующий элемент
	// заведёт свой.
	b.closeScreen(ctx, cs)
	if err := b.screen(c, msgResumingCase, nil); err != nil {
		return err
	}
	return c.Send(msgSendMaterial, collectKeyboard())
}

// projectOf - проект обращения либо nil, если он ещё не выбран.
func (b *Bot) projectOf(ctx context.Context, cs *Case) (*Project, error) {
	if cs.ProjectID == nil {
		return nil, nil
	}
	project, err := LoadProject(ctx, b.pool, *cs.ProjectID)
	if err != nil {
		return nil, err
	}
	return &project, nil
}

// onItem принимает любое сообщение автора; куда оно пойдёт, решает состояние
// обращения, а не последний экран: сырьё в сборе, ответ в разговоре. Сообщение
// без активного обращения не сохраняется, бот отвечает меню - решение владельца
// и его история в разделе 3 architecture.md.
func (b *Bot) onItem(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		// Одно меню на пачку: пересылка десяти сообщений дала бы десять меню
		// подряд. Апдейты идут последовательно, карта без мьютекса. Молчание
		// достаётся только пачке: набранное руками сообщение приходит по
		// одному, и тишина в ответ на него читается как «бот принял».
		if inBatch(c.Message()) && time.Since(b.menuAt[senderID(c)]) < menuRepeat {
			return nil
		}
		b.menuAt[senderID(c)] = time.Now()
		return b.homeScreen(ctx, c, msgNotSaved)
	}
	switch cs.Status {
	case statusInterview, statusSummary:
		return b.onAnswer(ctx, c, cs)
	case statusNormalizing, statusPublishing, statusAnswering:
		return b.sendState(c, cs)
	}
	return b.collect(ctx, c, cs)
}

// inBatch - сообщение пришло пачкой: пересылка или альбом. Bot API отдаёт их
// отдельными апдейтами за секунды, и отвечать на каждое незачем.
func inBatch(msg *tele.Message) bool {
	return msg != nil && (msg.IsForwarded() || msg.AlbumID != "")
}

// collect кладёт сообщение сырьём в обращение. Отдельно от onItem, потому что
// сырьём приходит и слово «Меню» из своего хендлера.
func (b *Bot) collect(ctx context.Context, c tele.Context, cs *Case) error {
	askProject, err := b.cases.CollectItem(ctx, b.bot, cs, c.Message())
	if err == nil {
		b.countItem(ctx, cs)
	}
	reply, internal := itemReply(err, b.maxItems)
	if reply != "" {
		if sendErr := c.Send(reply); sendErr != nil {
			return sendErr
		}
	}
	// Вопрос задаётся и при неудачном приёме: строка элемента уже вставлена, и
	// на следующем сообщении признак вопроса не сработает.
	if askProject {
		if askErr := b.askProject(ctx, c, msgAcceptedItem+projectAsk(cs)); askErr != nil {
			return askErr
		}
	}
	if internal {
		return err
	}
	return nil
}

// countItem - отклик на принятый материал: первое сообщение сбора заводит
// счётчик - живой экран без кнопок, следующие правят его же, не наращивая
// переписку. Число из базы: после рестарта или возврата в сбор память пуста и
// врала бы. Отклик косметический, ошибку наверх не отдаёт: шаги приёма из-за
// него не срываются.
func (b *Bot) countItem(ctx context.Context, cs *Case) {
	count, err := b.cases.CountItems(ctx, cs.ID)
	if err != nil {
		b.log.Warn("tally_count_failed", "case_id", cs.ID, "error", err)
		return
	}
	text := msgItemsAccepted(count)

	if cs.Screen != 0 {
		msg := tele.StoredMessage{MessageID: strconv.Itoa(cs.Screen), ChatID: cs.UserID}
		if _, err := b.bot.Edit(msg, text); err == nil || errors.Is(err, tele.ErrSameMessageContent) {
			return
		}
		// Сообщение удалили или оно слишком старое: заводим новый счётчик.
		b.log.Warn("tally_edit_failed", "case_id", cs.ID, "error", err)
	}

	if err := b.showStep(ctx, cs, 0, text); err != nil {
		b.log.Warn("tally_send_failed", "case_id", cs.ID, "error", err)
	}
}

// sendState объясняет автору, где стоит его обращение и чем оно двигается
// дальше. Один ответ на все входы, где человек упирается в занятый слот:
// активное обращение у автора одно, и без объяснения тупик читается как
// зависший бот. Выход - «Сброс» на панели, своя кнопка тут не нужна.
func (b *Bot) sendState(c tele.Context, cs *Case) error {
	// В сборе ответ заодно чинит панель: /start успел бы заменить её на
	// «Меню | Сброс», и «Готово» пропала бы с экрана.
	if cs.Status == statusCollecting {
		return c.Send(stateReply(cs.Status), collectKeyboard())
	}
	return c.Send(stateReply(cs.Status))
}

// onAnswer - ответ автора в разговоре. Голосовое уходит на расшифровку работой,
// текст записывается сразу: синхронный вызов модели остановил бы бота для всех
// авторов, апдейты обрабатываются последовательно.
func (b *Bot) onAnswer(ctx context.Context, c tele.Context, cs *Case) error {
	msg := c.Message()
	switch {
	case msg.Voice != nil:
		if err := b.cases.AddVoiceAnswer(ctx, b.bot, cs, msg); err != nil {
			if errors.Is(err, ErrFileTooBig) {
				return c.Send(msgVoiceTooBig)
			}
			if sendErr := c.Send(msgVoiceFailed); sendErr != nil {
				return sendErr
			}
			return err
		}
		b.waitFor(cs)
		return c.Send(msgTranscribing)
	case strings.TrimSpace(msg.Text) != "":
		if err := b.cases.AddAnswer(ctx, cs, msg.Text); err != nil {
			if errors.Is(err, ErrNotInterview) {
				return c.Send(msgCaseMovedOn)
			}
			return err
		}
		b.waitFor(cs)
		// До следующего раунда - секунды работы модели. Правим экран раунда:
		// кнопка «Всё так» снимается (ответ уже дан), и видно, что ответ принят.
		// Экран не тот (потерян рестартом, чужой раунд, отказ Telegram) -
		// отвечаем словами, молчания быть не должно.
		if !b.markRound(ctx, cs, msgAnswerAccepted) {
			return c.Send(msgAnswerAccepted)
		}
		return nil
	}
	return c.Send(msgAnswerNeedText)
}

// markRound переписывает экран раунда пометкой по числу ответов вместо нового
// сообщения на каждый ответ подряд. Правит только тот экран, что сейчас
// показывает раунд ответа (иначе правка сядет на саммари или на экран
// следующего раунда, ещё не доставленного - M1), и только если пометка не
// раздувает текст выше предела Telegram (S7). false - правки не будет, ответ
// идёт новым сообщением, его текст называет вызывающий.
func (b *Bot) markRound(ctx context.Context, cs *Case, first string) bool {
	if cs.Status != statusInterview || cs.Screen == 0 || cs.ScreenRound != cs.Round {
		return false
	}

	questions, answers, err := b.cases.RoundView(ctx, cs.ID)
	if err != nil {
		b.log.Warn("round_view_failed", "case_id", cs.ID, "error", err)
		return false
	}
	if len(questions) == 0 {
		return false
	}

	note := first
	if answers > 1 {
		note = msgAnswersAccepted(answers)
	}
	text := roundMessage(questions) + "\n\n---\n" + note
	if utf8.RuneCountInString(text) > maxMessage {
		return false
	}

	msg := tele.StoredMessage{MessageID: strconv.Itoa(cs.Screen), ChatID: cs.UserID}
	if _, err := b.bot.Edit(msg, text); err != nil {
		if errors.Is(err, tele.ErrSameMessageContent) {
			return true
		}
		b.log.Warn("round_mark_failed", "case_id", cs.ID, "error", err)
		return false
	}
	return true
}

// waitFor обещает автору отклик и предупреждает, если ход затянулся. Таймер, а
// не работа очереди: очередь занята ровно тем ходом, о задержке которого идёт
// речь, и сигнал через неё опоздал бы именно тогда, когда он нужен. Потеря
// таймера при рестарте безвредна - это уведомление, а не побочный эффект.
func (b *Bot) waitFor(cs *Case) {
	user, caseID, round := cs.UserID, cs.ID, cs.Round
	time.AfterFunc(waitNotice, func() {
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		defer cancel()

		fresh, err := b.cases.Load(ctx, caseID)
		if err != nil {
			b.log.Warn("wait_notice_failed", "case_id", caseID, "error", err)
			return
		}
		// Разговор ушёл дальше: вопрос показан, саммари собрано или обращение
		// закрыто. Предупреждать не о чем.
		waiting := fresh != nil && fresh.Round == round &&
			(fresh.Status == statusNormalizing || fresh.Status == statusInterview ||
				fresh.Status == statusAnswering)
		if !waiting || !b.markWaited(user, caseID, round) {
			return
		}
		// Срок называется с запасом: обещание «минуту» на восьмиминутном ходе
		// автор читает как поломку, и следом приходит жалоба на зависший бот.
		if _, err := b.bot.Send(&tele.User{ID: user}, msgStillWorking); err != nil {
			b.log.Warn("wait_notice_failed", "case_id", caseID, "error", err)
		}
		b.log.Info("wait_notice", "case_id", caseID, "round", round)
	})
}

// markWaited: одно предупреждение на ход. Автор отвечает несколькими
// сообщениями подряд, и каждое заводит свой таймер.
func (b *Bot) markWaited(user int64, caseID string, round int) bool {
	turn := caseID + "/" + strconv.Itoa(round)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.waited[user] == turn {
		return false
	}
	b.waited[user] = turn
	return true
}

// onAllTrue - «Всё так»: предположения модели становятся ответом целиком.
// Кнопка шага - хендлер читает обращение и сверяет его с живым экраном
// (правило 3) раньше своего тоста. Отказ Active наверх не отвечает на callback
// сам - это сделает OnError (S3); cs == nil - ожидаемый исход, а не сбой, и
// отвечает здесь же, иначе кнопка крутится до таймаута Telegram.
func (b *Bot) onAllTrue(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		// Callback не отвечен: OnError отвечает на него сам (bot.go), второй
		// Respond на тот же callback_query_id Telegram отклонит.
		return err
	}
	if cs == nil {
		b.toast(c, "")
		return b.screen(c, msgCaseClosed, backHome())
	}
	if !b.liveScreen(c, cs) {
		return nil
	}

	round, err := strconv.Atoi(c.Data())
	if err != nil {
		return b.staleButton(c)
	}

	// Тост зовёт каждая ветка сама, а не общий вызов до разбора ошибки: на
	// неизвестной ошибке AcceptRound хендлер обязан выйти без тоста вовсе -
	// ответит OnError, а тост здесь стал бы вторым ответом на тот же
	// callback_query_id. Устаревший раунд, отвеченный раунд и обращение не в
	// интервью - один и тот же факт для автора (правило 3 §2.1, M1): экран
	// устарел, staleButton - единственный ответ.
	switch acceptErr := b.cases.AcceptRound(ctx, cs, round); {
	case errors.Is(acceptErr, ErrRoundAnswered), errors.Is(acceptErr, ErrStaleRound), errors.Is(acceptErr, ErrNotInterview):
		b.log.Info("skip_refused", "case_id", cs.ID, "round", round, "reason", acceptErr.Error())
		return b.staleButton(c)
	case errors.Is(acceptErr, ErrNoSuggestion):
		b.toast(c, toastAccepted)
		return c.Send(msgNoSuggestion)
	case acceptErr != nil:
		return acceptErr
	}

	b.toast(c, toastAccepted)
	b.waitFor(cs)
	// Экран раунда правится тем же способом, что и типизированный ответ (S4):
	// счёт ответов один и честный, кто бы ни нажимал. Правка не вышла - ответ
	// уходит новым сообщением, а нажатая кнопка снимается: AcceptRound уже
	// принял ответ, второе нажатие той же кнопки его не изменит.
	if !b.markRound(ctx, cs, msgAllTrueAccepted) {
		if msg := c.Message(); msg != nil {
			b.stripScreen(cs, msg.ID)
		}
		return c.Send(msgAllTrueAccepted)
	}
	return nil
}

// onSkip - «Отправить как есть»: раунд закрывается без ответа автора, дальше
// саммари собирается тем, что уже сказано (Р-5, Р-15 ticket-form). Тот же
// порядок проверок, что у onAllTrue: Active раньше тоста (отказ отвечает
// OnError сам, S3). Номер раунда разбирается до liveScreen - порядок §5 плана
// среза.
func (b *Bot) onSkip(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		b.toast(c, "")
		return b.screen(c, msgCaseClosed, backHome())
	}

	round, err := strconv.Atoi(c.Data())
	if err != nil {
		return b.staleButton(c)
	}

	if !b.liveScreen(c, cs) {
		return nil
	}

	switch err := b.cases.SkipQuestions(ctx, cs, round); {
	case errors.Is(err, ErrNotInterview), errors.Is(err, ErrStaleRound), errors.Is(err, ErrRoundAnswered):
		// Раунд отвечен, пропущен раньше, не текущий или обращение ушло дальше -
		// автор видит один и тот же тост (решение 3 плана среза).
		return b.staleButton(c)
	case err != nil:
		return err
	}

	b.toast(c, toastAccepted)
	if err := b.screen(c, markAnswered(c, msgSkippedSummary), nil); err != nil {
		return err
	}
	b.waitFor(cs)
	return nil
}

// onToTicket - «Создать тикет» под ответом из документации. Тикет тут не
// заводится: разговор продолжается обычным интервью по уже сказанному, и
// подтверждение саммари остаётся на месте.
func (b *Bot) onToTicket(c tele.Context) error {
	b.toast(c, toastToTicket)

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		return b.screen(c, msgTalkClosedToTicket, backHome())
	}
	// Кнопка живёт в переписке дольше экрана: разговор мог стать тикетом сам,
	// репликой автора, или прошлым нажатием этой же кнопки. В разборе перевод
	// оборвал бы цепочку нормализации, и автор получает состояние вместо него.
	if cs.Mode != modeAsk || (cs.Status != statusCollecting && cs.Status != statusAnswering) {
		return b.sendState(c, cs)
	}

	switched, err := b.cases.SwitchToTicket(ctx, cs)
	if err != nil {
		return err
	}
	if !switched {
		return b.sendState(c, cs)
	}
	// Сбор закончился: живой экран сбора больше не этого обращения. Панель
	// сбора снимает сообщение о переводе - оно идёт из очереди, одинаково для
	// кнопки и для реплики, распознанной ходом lookup.
	b.closeScreen(ctx, cs)
	b.waitFor(cs)
	return b.screen(c, markAnswered(c, msgToTicketDone), nil)
}

// onEndAsk - «Закончить разговор»: вопрос закрыт ответом, слот активного
// обращения свободен, автор возвращается в меню.
func (b *Bot) onEndAsk(c tele.Context) error {
	b.toast(c, toastEndingAsk)

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		return b.screen(c, msgTalkAlreadyEnded, backHome())
	}
	// Разговор уже стал тикетом: этой кнопкой его не закрыть, выход - «Сброс».
	if cs.Mode != modeAsk {
		return b.sendState(c, cs)
	}

	if err := b.cases.EndAsk(ctx, cs); err != nil {
		return err
	}
	b.closeScreen(ctx, cs)
	if err := b.screen(c, markAnswered(c, msgTalkEnded), nil); err != nil {
		return err
	}
	if err := b.sendPanel(c, msgReadyWhatNext); err != nil {
		return err
	}
	return b.homeScreen(ctx, c, "")
}

// onPublish - «Публикую»: подтверждение саммари, после которого тикет уходит в
// GitHub, а файлы обращения удаляются. Кнопка шага - обращение читается и
// сверяется с живым экраном (правило 3) раньше своего тоста. Отказ Active
// наверх не отвечает на callback сам - это сделает OnError (S3); cs == nil -
// ожидаемый исход, а не сбой, и отвечает здесь же.
func (b *Bot) onPublish(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		b.toast(c, "")
		return b.screen(c, msgCaseClosed, backHome())
	}
	if !b.liveScreen(c, cs) {
		return nil
	}
	b.toast(c, toastPublishing)

	if err := b.cases.ConfirmSummary(ctx, cs); err != nil {
		if errors.Is(err, ErrNoSummary) {
			// Кнопка осталась в чате, а обращение ушло дальше - или это уже
			// другое обращение того же автора: слот один, кнопка вечная.
			switch cs.Status {
			case statusInterview:
				return c.Send(msgSummaryRewriting)
			case statusPublishing:
				return c.Send(msgAlreadyPublishing)
			}
			return b.sendState(c, cs)
		}
		return err
	}
	// Кнопки саммари снимаются: тикет уже уходит, второе нажатие только
	// путало бы. Номер придёт отдельным сообщением из очереди.
	return b.screen(c, markAnswered(c, msgPublishingNow), nil)
}

// onFix - «Поправить»: состояние не меняет, правкой становится следующее
// сообщение автора через тот же onAnswer (раздел 3 architecture.md). Гвардия
// обязательна: кнопка живёт дольше обращения, и приглашение «напишите правку»
// увело бы следующий текст автора в сырьё нового обращения. Кнопка шага -
// обращение читается и сверяется с живым экраном (правило 3) раньше своего
// тоста. Отказ Active наверх не отвечает на callback сам - это сделает OnError
// (S3); cs == nil - ожидаемый исход, а не сбой, и отвечает здесь же.
func (b *Bot) onFix(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		b.toast(c, "")
		return b.screen(c, msgCaseClosed, backHome())
	}
	if !b.liveScreen(c, cs) {
		return nil
	}
	b.toast(c, toastAwaitingFix)

	if !inDialog(cs.Status) {
		return b.sendState(c, cs)
	}
	// Кнопка «Публикую» остаётся: автор мог передумать править, и без неё
	// пришлось бы искать саммари выше по переписке.
	markup := &tele.ReplyMarkup{}
	markup.Inline(markup.Row(markup.Data(buttonPublish, publishBtn.Unique)))
	text := markAnswered(c, msgAwaitingFixNote)

	// editScreen зовётся напрямую, а не через screen(): отказ правки саммари
	// (M2) уводит screen_msg на запасное сообщение - иначе «Публикую» на
	// старом, но всё ещё видимом сообщении читалось бы как живая кнопка.
	sent, err := b.editScreen(c.Recipient(), c.Message(), text, markup)
	if err != nil {
		return err
	}
	if sent != nil {
		return b.cases.SetScreen(ctx, cs.ID, sent.ID, 0)
	}
	return nil
}

func (b *Bot) onDone(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		return c.Send(msgNothingToFinish, homeKeyboard())
	}
	// Слово «Готово» в разговоре - обычный ответ («статус заказа - Готово»), а
	// не команда сбора: reply-кнопка маршрутизируется по точному тексту раньше
	// OnText, и без этой развилки ответ автора пропадал бы молча.
	if !isCommand(c) && inDialog(cs.Status) {
		return b.onAnswer(ctx, c, cs)
	}

	switch err := b.cases.FinishCollect(ctx, cs); {
	case errors.Is(err, ErrNoItems):
		return c.Send(msgNothingToParse)
	case errors.Is(err, ErrNoNewItems):
		// Сырьё прошлого вопроса на месте, нового нет: молчание тут читалось бы
		// как принятое «Готово», а второй поход в документацию повторил бы ответ.
		return c.Send(msgNoNewQuestion)
	case errors.Is(err, ErrNoProject):
		// Сырьё на месте, статус не менялся: не хватает только проекта.
		return b.askProject(ctx, c, projectAsk(cs))
	case errors.Is(err, ErrNotCollecting):
		// Статусный ответ вместо «разбираю»: /done в нормализации и правда
		// значит «идёт разбор», а в разговоре бот ждёт автора, а не наоборот.
		return b.sendState(c, cs)
	case err != nil:
		return err
	}
	// Сбор закрыт: живой экран счётчика отработал, следующее обращение заведёт
	// свой.
	b.closeScreen(ctx, cs)
	b.waitFor(cs)
	// В режиме вопроса панель сбора остаётся: следующий вопрос задаётся тем же
	// порядком, «написал - нажал Готово», и искать её заново автор не должен.
	if cs.Mode == modeAsk {
		return c.Send(msgQuestionAccepted, collectKeyboard())
	}
	return c.Send(msgParsingMaterial, homeKeyboard())
}

// onMenu - «Меню» с панели: показать начало или объяснить, где автор стоит.
// В сборе слово уходит сырьём: на панели сбора кнопки «Меню» нет, значит это
// текст материала, и красть его нельзя.
func (b *Bot) onMenu(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		return b.homeScreen(ctx, c, msgLetsStart)
	}
	if cs.Status == statusCollecting {
		return b.collect(ctx, c, cs)
	}
	return b.sendState(c, cs)
}

// onReset - глобальный «Сброс» и /cancel: отменить активное обращение и
// вернуть в начало. Обращение с материалом переспрашивает: reply-кнопка
// нажимается одним касанием, а сброс стирает обращение с файлами безвозвратно.
func (b *Bot) onReset(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		if err := b.sendPanel(c, msgNothingToReset); err != nil {
			return err
		}
		return b.homeScreen(ctx, c, "")
	}
	if cs.Status == statusPublishing {
		// Работа publish уже в очереди: погасив обращение здесь, мы получили
		// бы issue по отменённому.
		return c.Send(msgTicketLeaving)
	}

	// Пустой сбор сбрасывается без подтверждения: терять нечего.
	count, err := b.cases.CountItems(ctx, cs.ID)
	if err != nil {
		return err
	}
	if count > 0 {
		// Подтверждение привязано к обращению: кнопка живёт в переписке дольше,
		// чем слот, и без привязки отменяла бы уже другое обращение автора.
		markup := &tele.ReplyMarkup{}
		markup.Inline(
			markup.Row(markup.Data(buttonResetYes, resetYesBtn.Unique, cs.ID)),
			markup.Row(markup.Data(buttonResetNo, resetNoBtn.Unique)),
		)
		return c.Send(msgConfirmReset, markup)
	}
	if err := b.cases.CancelCase(ctx, cs, "reset"); err != nil {
		return err
	}
	// Обращение ушло целиком: живой экран больше не его.
	b.closeScreen(ctx, cs)
	if err := b.sendPanel(c, msgResetDone); err != nil {
		return err
	}
	return b.homeScreen(ctx, c, "")
}

// onResetYes - подтверждение сброса. Кнопка живёт в переписке дольше экрана:
// обращение могло уйти дальше или закрыться, отменяется то, что активно
// сейчас. Тост «Сбрасываю» ставится после разбора (порядок §5 плана среза):
// устаревшее подтверждение отвечает staleButton, а он и есть единственный
// ответ на нажатие - второй тост Telegram отклонил бы.
func (b *Bot) onResetYes(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		b.toast(c, "")
		return b.screen(c, msgCaseClosed, backHome())
	}
	if c.Data() != cs.ID {
		return b.staleButton(c)
	}
	b.toast(c, toastResetting)

	if err := b.cases.CancelCase(ctx, cs, "reset"); err != nil {
		if errors.Is(err, ErrPublishing) {
			return b.screen(c, msgTicketLeaving, nil)
		}
		return err
	}
	// Обращение ушло целиком: живой экран больше не его.
	b.closeScreen(ctx, cs)
	if err := b.screen(c, msgCaseReset, nil); err != nil {
		return err
	}
	if err := b.sendPanel(c, msgStartOver); err != nil {
		return err
	}
	return b.homeScreen(ctx, c, "")
}

func (b *Bot) onResetNo(c tele.Context) error {
	b.toast(c, toastKeeping)
	// Правка вместо нового сообщения: кнопки подтверждения снимаются, чтобы
	// «Да, сбросить» не сработала неделю спустя.
	return b.screen(c, msgKeptGoing, nil)
}

// isCommand отличает набранную команду от текста reply-кнопки: у них общий
// хендлер, а смысл разный.
func isCommand(c tele.Context) bool {
	return strings.HasPrefix(strings.TrimSpace(c.Text()), "/")
}

// inDialog - статусы, в которых любой текст автора считается ответом.
func inDialog(status string) bool {
	return status == statusInterview || status == statusSummary
}

// Панель управления. Reply-клавиатура, а не инлайн: её видно всегда и не надо
// искать кнопку выше по переписке. Раскладок две: вне сбора и в сборе.
func homeKeyboard() *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{ResizeKeyboard: true, IsPersistent: true}
	markup.Reply(markup.Row(*menuBtn, *resetBtn))
	return markup
}

func collectKeyboard() *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{ResizeKeyboard: true, IsPersistent: true}
	markup.Reply(markup.Row(*doneBtn, *resetBtn))
	return markup
}

// roundKeyboard - кнопки под раундом вопросов: «Отправить как есть» есть
// всегда (R5 ticket-form), «Всё так» - только когда есть догадка, которую она
// подтверждает (иначе обещание кнопки разошлось бы с текстом). Номер раунда
// уезжает в callback_data обеих: кнопки прошлых раундов остаются в переписке,
// и по нажатию надо понять, к каким вопросам оно относится.
func roundKeyboard(round int, suggested bool) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	data := strconv.Itoa(round)
	row := make([]tele.Btn, 0, 2)
	if suggested {
		row = append(row, markup.Data(buttonAllTrue, allTrueBtn.Unique, data))
	}
	row = append(row, markup.Data(buttonSkip, skipBtn.Unique, data))
	markup.Inline(markup.Row(row...))
	return markup
}

// summaryKeyboard - решение по саммари. Отмены здесь нет: её несёт «Сброс» на
// панели, и один путь выхода лучше двух одинаковых.
func summaryKeyboard() *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	markup.Inline(markup.Row(
		markup.Data(buttonPublish, publishBtn.Unique),
		markup.Data(buttonFix, fixBtn.Unique),
	))
	return markup
}

// answerKeyboard - решение по ответу из документации. Продолжить разговор
// кнопки не предлагают: следующий вопрос идёт тем же порядком, что и первый.
func answerKeyboard() *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	markup.Inline(
		markup.Row(markup.Data(buttonCreateTicket, toTicketBtn.Unique)),
		markup.Row(markup.Data(buttonEndAsk, endAskBtn.Unique)),
	)
	return markup
}

// onTicketList - вход в просмотр командой. Нужен отдельно от кнопки меню: меню
// действий рисуется только автору без активного обращения, а вспоминает про
// старый тикет человек как раз посреди интервью.
func (b *Bot) onTicketList(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	return b.askProjectFor(ctx, c, msgWhichProjectTickets, ticketsBtn)
}

// onTickets - список тикетов проекта. Список, карточка и возврат к списку
// живут одним сообщением-экраном: оно переписывается на каждом шаге, и автор
// видит отклик там же, куда смотрит.
func (b *Bot) onTickets(c tele.Context) error {
	b.toast(c, toastOpeningTickets)

	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	slug, page := listTarget(c.Data())
	project, ok, err := b.project(ctx, slug)
	if err != nil {
		return err
	}
	if !ok {
		return b.screen(c, msgProjectDisabled, backHome())
	}
	return b.showTickets(ctx, c, project, page)
}

// showTickets рисует страницу списка. Вынесена из хендлера, потому что страница
// за концом списка возвращает автора на первую: тикет могли отменить между
// показом и нажатием.
func (b *Bot) showTickets(ctx context.Context, c tele.Context, project Project, page int) error {
	tickets, total, err := b.tickets.List(ctx, project, senderID(c), page)
	if err != nil {
		return err
	}
	if len(tickets) == 0 {
		if page > 0 {
			return b.showTickets(ctx, c, project, 0)
		}
		return b.screen(c, msgNoTickets(project.Title),
			backProject(project.Slug))
	}
	pages := (total + ticketsLimit - 1) / ticketsLimit
	b.log.Info("tickets_listed", "user_id", senderID(c), "project", project.Slug,
		"count", len(tickets), "page", page+1, "pages", pages)

	markup := &tele.ReplyMarkup{}
	rows := make([]tele.Row, 0, len(tickets)+2)
	var text strings.Builder
	text.WriteString(msgTicketsOfProject(project.Title))
	if pages > 1 {
		fmt.Fprintf(&text, msgTicketsPageSuffix, page+1, pages)
	}
	text.WriteString(":\n\n")
	for _, t := range tickets {
		text.WriteString(ticketLine(t) + "\n")
		rows = append(rows, markup.Row(markup.Data(
			ticketButton(t), cardBtn.Unique, cardData(project.Slug, t.Number))))
	}
	// Кнопка листания появляется только там, куда есть куда идти: кнопка,
	// которая ничего не делает, читается как поломка.
	var nav tele.Row
	if page > 0 {
		nav = append(nav, markup.Data(buttonPrevPage, ticketsBtn.Unique, listData(project.Slug, page-1)))
	}
	if page+1 < pages {
		nav = append(nav, markup.Data(buttonNextPage, ticketsBtn.Unique, listData(project.Slug, page+1)))
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, markup.Row(
		markup.Data(buttonProjectMenu, backBtn.Unique, project.Slug),
		markup.Data(buttonToProjects, homeBtn.Unique)))
	markup.Inline(rows...)
	return b.screen(c, text.String(), markup)
}

// onCard - карточка тикета. Кнопка отмены только автору: просмотр общий, отмена
// нет.
func (b *Bot) onCard(c tele.Context) error {
	slug, number, ok := parseCard(c.Data())
	if !ok {
		b.log.Warn("bad_card_data", "user_id", senderID(c), "data", c.Data())
		return b.staleButton(c)
	}
	b.toast(c, toastOpeningTicket)

	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	project, ok, err := b.cardTarget(ctx, c, slug)
	if err != nil || !ok {
		return err
	}
	page, err := b.tickets.Page(ctx, project, number)
	if err != nil {
		return err
	}

	ticket, err := b.tickets.Load(ctx, project, number)
	switch {
	case errors.Is(err, ErrIssueGone):
		return b.screen(c, msgTicketGoneOnCard(number), backToList(project.Slug, page))
	case err != nil:
		return err
	case ticket == nil:
		return b.screen(c, msgTicketNotFound, backToList(project.Slug, page))
	}
	b.log.Info("ticket_opened", "user_id", senderID(c), "case_id", ticket.CaseID, "issue", number)

	// Новость прочитана: карточка показывает и статус, и комментарий, а отметка в
	// списке дальше только мешала бы искать непрочитанное.
	if ticket.News && ticket.UserID == senderID(c) {
		if err := b.tickets.MarkSeen(ctx, ticket.CaseID, senderID(c)); err != nil {
			return err
		}
	}

	markup := &tele.ReplyMarkup{}
	var rows []tele.Row
	if cancelOffered(ticket, senderID(c)) {
		rows = append(rows, markup.Row(markup.Data(buttonCancelTicket, killBtn.Unique,
			cardData(project.Slug, number))))
	}
	rows = append(rows, markup.Row(
		markup.Data(buttonBackToList, ticketsBtn.Unique, listData(project.Slug, page)),
		markup.Data(buttonProjectMenu, backBtn.Unique, project.Slug)))
	markup.Inline(rows...)

	return b.screen(c, cardText(ticket), markup)
}

// backToList - единственная кнопка возврата к списку тикетов проекта. Страница
// та, на которой лежит тикет: возврат в начало списка с третьей страницы читался
// бы как потеря места.
func backToList(slug string, page int) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	markup.Inline(markup.Row(markup.Data(buttonBackToList, ticketsBtn.Unique, listData(slug, page))))
	return markup
}

// onKill ставит работу отмены. Ответ автору синхронный, сама отмена идёт
// очередью: это мутация в GitHub, она обязана пережить рестарт. Порядок:
// разбор кнопки, отказ - staleButton и выход, затем тост действия (§5 плана
// среза) - иначе второй ответ на callback Telegram отклонит.
func (b *Bot) onKill(c tele.Context) error {
	slug, number, ok := parseCard(c.Data())
	if !ok {
		b.log.Warn("bad_card_data", "user_id", senderID(c), "data", c.Data())
		return b.staleButton(c)
	}
	b.toast(c, toastCancellingTicket)

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	project, ok, err := b.cardTarget(ctx, c, slug)
	if err != nil || !ok {
		return err
	}
	page, err := b.tickets.Page(ctx, project, number)
	if err != nil {
		return err
	}

	// Экран запоминается в той же транзакции, что и постановка работы
	// (Tickets.Cancel, Р-8): исход придёт очередью и перепишет это же
	// сообщение по cases.screen_msg, а не по памяти процесса.
	msgID := 0
	if msg := c.Message(); msg != nil {
		msgID = msg.ID
	}
	_, err = b.tickets.Cancel(ctx, project, number, senderID(c), msgID)
	switch {
	case errors.Is(err, ErrNotAuthor):
		b.log.Warn("cancel_denied", "user_id", senderID(c), "project", project.Slug, "issue", number)
		return b.screen(c, msgCancelNotAuthor, backToList(project.Slug, page))
	case errors.Is(err, ErrIssueGone):
		return b.screen(c, msgTicketNotFound, backToList(project.Slug, page))
	case err != nil:
		return err
	}
	// Кнопка отмены снимается сразу: работа уже в очереди, второе нажатие
	// поставило бы её заново.
	return b.screen(c, msgCancellingTicket(number),
		backToList(project.Slug, page))
}

// parseCard разбирает callback_data карточки тикета ("slug:number").
func parseCard(data string) (slug string, number int, ok bool) {
	s, raw, found := strings.Cut(data, ":")
	n, err := strconv.Atoi(raw)
	if !found || err != nil {
		return "", 0, false
	}
	return s, n, true
}

// cardTarget резолвит проект карточки по уже разобранному slug (parseCard) -
// проект, выключенный между показом списка и нажатием, отвечает отказом.
func (b *Bot) cardTarget(ctx context.Context, c tele.Context, slug string) (Project, bool, error) {
	project, ok, err := b.project(ctx, slug)
	if err != nil {
		return Project{}, false, err
	}
	if !ok {
		return Project{}, false, b.screen(c, msgProjectDisabled, backHome())
	}
	return project, true, nil
}

// project резолвит slug кнопки. Фильтр active тот же, что у меню: проект,
// выключенный между открытием списка и нажатием, отвечает отказом.
func (b *Bot) project(ctx context.Context, slug string) (Project, bool, error) {
	projects, err := ListProjects(ctx, b.pool)
	if err != nil {
		return Project{}, false, err
	}
	index := slices.IndexFunc(projects, func(p Project) bool { return p.Slug == slug })
	if index < 0 {
		return Project{}, false, nil
	}
	return projects[index], true, nil
}

func cardData(slug string, number int) string {
	return slug + ":" + strconv.Itoa(number)
}

// listData - кнопка страницы списка. Формат тот же, что у карточки: `slug:N`
// укладывается в 64 байта callback_data с запасом.
func listData(slug string, page int) string {
	return slug + ":" + strconv.Itoa(page)
}

// listTarget разбирает кнопку списка. Кнопка без номера страницы - это вход из
// меню проекта и из команды: там страница всегда первая.
func listTarget(data string) (string, int) {
	slug, raw, found := strings.Cut(data, ":")
	if !found {
		return data, 0
	}
	page, err := strconv.Atoi(raw)
	if err != nil || page < 0 {
		return slug, 0
	}
	return slug, page
}

// cancelOffered - показывать ли кнопку отмены. Отменяет только автор и только
// пока статус прочитан: при молчащем GitHub «не доигран» - это незнание, а не
// факт, и кнопка звала бы отменять давно закрытый тикет.
func cancelOffered(t *Ticket, userID int64) bool {
	return t.UserID == userID && !t.Unavailable && !t.Status.Final
}

// cardComment - сколько комментария помещается в карточку. Разбор разработчика
// бывает в тысячи символов: без предела карточка не влезала в одно сообщение и
// уезжала автору двумя, вторым - обрывок комментария.
const cardComment = 600

// onProjectAdd - команда /project: быстрый путь для того, кто ссылку уже
// скопировал. Заводить может любой из белого списка: ролей в сервисе нет, а
// токен всё равно ограничен своими репозиториями.
func (b *Bot) onProjectAdd(c tele.Context) error {
	args := strings.TrimSpace(c.Message().Payload)
	if args == "" {
		return c.Send(projectHelp)
	}
	if err := b.addProject(c, args); err != nil {
		if errors.Is(err, ErrBadProjectRef) {
			return c.Send(projectHelp)
		}
		return b.refuseProject(c, err)
	}
	return nil
}

// onAddProject - кнопка «Добавить проект»: следующее сообщение автора - ссылка.
// При активном обращении кнопка не работает: ссылка ушла бы в него сырьём.
func (b *Bot) onAddProject(c tele.Context) error {
	b.toast(c, toastAwaitingLink)

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs != nil {
		return b.sendState(c, cs)
	}
	b.awaitLink[senderID(c)] = time.Now()
	return b.screen(c, msgSendRepoLink, nil)
}

// onProjectLink - сообщение, обещанное после «Добавить проект». Зовёт allow:
// хендлера у него нет, ожидание разбирается до маршрутизации.
func (b *Bot) onProjectLink(c tele.Context) error {
	err := b.addProject(c, strings.TrimSpace(c.Text()))
	if err == nil {
		return nil
	}
	// Обещание ссылки продлевается на любом отказе: автор ошибся, а не
	// передумал. Только здесь: команда /project с мусором ждать следующее
	// сообщение не обещала, и её ожидание крало бы сырьё активного сбора.
	b.awaitLink[senderID(c)] = time.Now()
	if errors.Is(err, ErrBadProjectRef) {
		return c.Send(projectHelp)
	}
	return b.refuseProject(c, err)
}

// addProject заводит проект и отвечает карточкой. Отказы возвращает как есть:
// ответ на них каждый вход даёт свой.
func (b *Bot) addProject(c tele.Context, args string) error {
	// Бюджет с запасом: чтение репозитория, README и ход модели.
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	project, source, err := b.projects.Add(ctx, senderID(c), args)
	if err != nil {
		return err
	}
	_, err = b.sendLong(c.Recipient(), projectCard(project, source))
	return err
}

// refuseProject отвечает на отказ заведения проекта; ErrBadProjectRef входы
// разбирают сами, до этого ответа он не доходит.
func (b *Bot) refuseProject(c tele.Context, err error) error {
	if errors.Is(err, ErrSlugTaken) {
		return c.Send(msgSlugTaken)
	}
	b.log.Warn("project_add_failed", "user_id", senderID(c), "error", err)
	return c.Send(projectFailText(err))
}
