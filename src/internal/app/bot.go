package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
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
	// не увидел бы результат вовсе.
	maxMessage = 4000
	// Пачка пересылок без активного обращения приходит за секунды; меню в
	// ответ на неё должно быть одно, а не по числу сообщений.
	menuRepeat = 10 * time.Second
	// Сколько живёт обещание «пришлю ссылку» после «Добавить проект».
	linkWait = 10 * time.Minute
)

// Инлайн-кнопки: telebot маршрутизирует callback по Unique и кодирует кнопку
// как \f<unique>|<data>. Свой формат callback_data мимо Unique хендлером не
// поймается. Кнопки сбора - reply-клавиатура, они маршрутизируются по тексту,
// поэтому один и тот же Btn идёт и в Handle, и в клавиатуру.
var (
	projectBtn    = &tele.Btn{Unique: "project"}
	createBtn     = &tele.Btn{Unique: "create"}
	continueBtn   = &tele.Btn{Unique: "continue"}
	allTrueBtn    = &tele.Btn{Unique: "all_true"}
	publishBtn    = &tele.Btn{Unique: "publish"}
	fixBtn        = &tele.Btn{Unique: "fix"}
	ticketsBtn    = &tele.Btn{Unique: "tickets"}
	cardBtn       = &tele.Btn{Unique: "card"}
	killBtn       = &tele.Btn{Unique: "kill"}
	addProjectBtn = &tele.Btn{Unique: "add_project"}
	resetYesBtn   = &tele.Btn{Unique: "reset_yes"}
	resetNoBtn    = &tele.Btn{Unique: "reset_no"}
	doneBtn       = &tele.Btn{Text: "Готово"}
	menuBtn       = &tele.Btn{Text: "Меню"}
	resetBtn      = &tele.Btn{Text: "Сброс"}
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
	// Когда автору в последний раз отвечали меню на свободное сообщение.
	// Хендлеры идут последовательно (Synchronous), мьютекс не нужен; потеря
	// при рестарте безвредна - автор получит меню лишний раз.
	menuAt map[int64]time.Time
	// Ожидание ссылки после «Добавить проект»: следующий текст автора - ссылка
	// на репозиторий. Та же дисциплина, что и menuAt: последовательные хендлеры,
	// потеря при рестарте безвредна - автор нажмёт кнопку снова.
	awaitLink map[int64]time.Time
}

func NewBot(ctx context.Context, cfg Config, pool *pgxpool.Pool, cases *Cases, tickets *Tickets, projects *Projects, log *slog.Logger) (*Bot, error) {
	b := &Bot{pool: pool, cases: cases, tickets: tickets, projects: projects, log: log,
		allowed: cfg.AllowedIDs, maxItems: cfg.MaxItems,
		menuAt: map[int64]time.Time{}, awaitLink: map[int64]time.Time{}}

	// Verbose дампит сырые payload Bot API вместе с текстами сообщений, а
	// стандартный OnError пишет через stdlib log мимо JSON.
	//
	// Synchronous обязателен: по умолчанию каждый хендлер уходит в свою
	// горутину, и пачка из десяти пересылок даёт параллельные хендлеры над
	// одним обращением - двойную вставку cases, случайный порядок элементов в
	// протоколе и повторный вопрос о проекте.
	tb, err := startBot(ctx, tele.Settings{
		Token:       cfg.BotToken,
		Poller:      &tele.LongPoller{Timeout: 10 * time.Second},
		Client:      proxyClient(cfg.TelegramProxy),
		Verbose:     false,
		Synchronous: true,
		OnError: func(err error, c tele.Context) {
			log.Error("handler_failed", "error", err, "user_id", senderID(c))
		},
	}, log)
	if err != nil {
		return nil, err
	}

	tb.Use(b.allow)
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
	tb.Handle(continueBtn, b.onContinue)
	tb.Handle(allTrueBtn, b.onAllTrue)
	tb.Handle(publishBtn, b.onPublish)
	tb.Handle(fixBtn, b.onFix)
	tb.Handle(addProjectBtn, b.onAddProject)
	tb.Handle(resetYesBtn, b.onResetYes)
	tb.Handle(resetNoBtn, b.onResetNo)
	tb.Handle(doneBtn, b.onDone)
	tb.Handle(menuBtn, b.onMenu)
	tb.Handle(resetBtn, b.onReset)

	// Кнопки прежних версий («Отмена» под саммари, «Начать заново») хендлеров
	// больше не имеют; без фолбэка их нажатие - вечный спиннер и вид зависшего
	// бота. Сюда же падает любой незнакомый callback.
	tb.Handle(tele.OnCallback, func(c tele.Context) error {
		if err := c.Respond(); err != nil {
			return err
		}
		return c.Send("Кнопка устарела. Нажмите «Меню» и продолжите оттуда.")
	})

	// Меню «/» в клиенте. Отказ не роняет старт: бот работает и без него.
	if err := tb.SetCommands([]tele.Command{
		{Text: "start", Description: "Меню"},
		{Text: "tickets", Description: "Мои тикеты"},
		{Text: "project", Description: "Добавить проект"},
		{Text: "cancel", Description: "Сброс"},
	}); err != nil {
		log.Warn("set_commands_failed", "error", err)
	}

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

	b.bot = tb
	return b, nil
}

// proxyClient - клиент Bot API. Пустой адрес прокси даёт nil: telebot возьмёт
// свой клиент, и прямой путь останется как был.
//
// net/http понимает и http, и socks5 в адресе прокси, поэтому SOCKS-туннель
// соседних сервисов работает без пятой зависимости. Таймаут с запасом над
// циклом опроса: long polling держит соединение открытым по десять секунд, а
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

	cs, err := b.cases.Load(ctx, p.CaseID)
	if err != nil {
		return err
	}
	if cs == nil {
		return fmt.Errorf("notify: case %s not found", p.CaseID)
	}

	var opts []any
	switch {
	// Провал нормализации возвращает обращение в сбор, а кнопки сбора сняты
	// нажатием «Готово»: без них автор не поймёт, чем закончить второй заход.
	case cs.Status == statusCollecting:
		opts = append(opts, collectKeyboard())
	case p.Buttons == keysRound:
		opts = append(opts, roundKeyboard(cs.Round))
	case p.Buttons == keysSummary:
		opts = append(opts, summaryKeyboard())
	case p.Buttons == keysHome:
		opts = append(opts, homeKeyboard())
	}
	return b.sendLong(&tele.User{ID: cs.UserID}, p.Text, opts...)
}

// sendLong режет длинный текст по границе строки: протокол не влезает в одно
// сообщение, а отказ Telegram увёл бы работу в повторы.
func (b *Bot) sendLong(to tele.Recipient, text string, opts ...any) error {
	for {
		if len(text) <= maxMessage {
			_, err := b.bot.Send(to, text, opts...)
			if err != nil {
				return fmt.Errorf("send message: %w", err)
			}
			return nil
		}

		cut := strings.LastIndex(text[:maxMessage], "\n")
		if cut <= 0 {
			cut = maxMessage
			for cut > 1 && !utf8.RuneStart(text[cut]) {
				cut--
			}
		}
		// opts только на последнем куске: иначе кнопки дублируются под каждым, и
		// автор жмёт устаревшую копию выше по переписке.
		if _, err := b.bot.Send(to, strings.TrimRight(text[:cut], "\n")); err != nil {
			return fmt.Errorf("send message part: %w", err)
		}
		text = strings.TrimLeft(text[cut:], "\n")
	}
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
			return refuse(c, "Бот работает только в личной переписке.")
		}
		if !slices.Contains(b.allowed, sender.ID) {
			b.log.Warn("access_denied", "user_id", sender.ID)
			return refuse(c, "Доступ к боту закрыт. Напишите владельцу сервиса.")
		}
		// Ожидание ссылки после «Добавить проект» живёт до первого апдейта:
		// скрытый режим не должен пережить смену темы. Слова панели пропускаются
		// к своим хендлерам - нажатие «Меню» не должно разбираться как ссылка.
		if since, ok := b.awaitLink[sender.ID]; ok {
			delete(b.awaitLink, sender.ID)
			text := strings.TrimSpace(c.Text())
			if c.Callback() == nil && text != "" && !isCommand(c) && !panelWord(text) &&
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
	return b.homeScreen(ctx, c, "Я завожу тикеты по обращениям сотрудников.")
}

// homeScreen - начальная точка: панель управления и экран проектов. Единая для
// /start, «Меню», «Сброса» без обращения и любого свободного сообщения.
//
// Два сообщения, потому что у Telegram reply-панель и инлайн-кнопки не живут в
// одном: первое ставит панель, второе несёт экран проектов.
func (b *Bot) homeScreen(ctx context.Context, c tele.Context, intro string) error {
	if err := c.Send(intro, homeKeyboard()); err != nil {
		return err
	}

	projects, err := ListProjects(ctx, b.pool)
	if err != nil {
		return err
	}
	markup := &tele.ReplyMarkup{}
	rows := make([]tele.Row, 0, len(projects)+1)
	for _, p := range projects {
		rows = append(rows, markup.Row(markup.Data(p.Title, projectBtn.Unique, p.Slug)))
	}
	rows = append(rows, markup.Row(markup.Data("Добавить проект", addProjectBtn.Unique)))
	markup.Inline(rows...)
	return c.Send("Выберите проект или добавьте новый:", markup)
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
		markup.Inline(markup.Row(markup.Data("Добавить проект", addProjectBtn.Unique)))
		return c.Send("Проекты ещё не заведены. Добавьте первый:", markup)
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
	if err := c.Respond(); err != nil {
		return err
	}

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
		return c.Send("Проект недоступен. Отправьте /start и выберите заново.")
	}
	title := projects[index].Title

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	b.log.Info("project_selected", "user_id", senderID(c), "slug", slug)

	if cs == nil {
		markup := &tele.ReplyMarkup{}
		markup.Inline(
			markup.Row(markup.Data("Создать тикет", createBtn.Unique, slug)),
			markup.Row(markup.Data("Посмотреть тикеты", ticketsBtn.Unique, slug)),
		)
		return c.Send("Проект «"+title+"» выбран.", markup)
	}

	if err := b.cases.SetProject(ctx, cs, slug); err != nil {
		if errors.Is(err, ErrNotCollecting) {
			return b.sendState(c, cs)
		}
		if errors.Is(err, ErrUnknownProject) {
			return c.Send("Проект недоступен. Отправьте /start и выберите заново.")
		}
		return err
	}
	return c.Send("Проект «"+title+"» выбран. Присылайте материал и нажмите «Готово».", collectKeyboard())
}

// onCreate - «Создать тикет». При живом обращении автор выбирает: продолжить
// его или начать заново. Активное обращение у автора одно, и молча подменять
// его нельзя - в нём уже лежит сырьё.
func (b *Bot) onCreate(c tele.Context) error {
	if err := c.Respond(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	slug := c.Data()
	cs, existed, err := b.cases.StartCase(ctx, author(c), slug)
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
		markup.Inline(markup.Row(markup.Data("Продолжить", continueBtn.Unique, slug)))
		return c.Send("У вас уже есть незакрытое обращение. Продолжайте его "+
			"или нажмите «Сброс», чтобы начать заново.", markup)
	}
	return c.Send("Присылайте текст, голосовые и скриншоты. Когда закончите, нажмите «Готово».", collectKeyboard())
}

func (b *Bot) onContinue(c tele.Context) error {
	if err := c.Respond(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		return c.Send("Обращение уже закрыто. Отправьте /start и заведите новое.")
	}
	if cs.Status != statusCollecting {
		return b.sendState(c, cs)
	}

	// Автор пришёл из меню конкретного проекта, значит продолжать надо в нём:
	// проект можно менять до «Готово», источник истины - строка в cases.
	if slug := c.Data(); slug != "" {
		if err := b.cases.SetProject(ctx, cs, slug); err != nil && !errors.Is(err, ErrUnknownProject) {
			return err
		}
	}
	return c.Send("Продолжаем. Присылайте материал и нажмите «Готово».", collectKeyboard())
}

// onItem принимает любое сообщение автора. Куда оно пойдёт, решает состояние
// обращения, а не то, какой экран автор видел последним: сырьё в сборе, ответ в
// разговоре. Без этой развилки ответ на вопрос интервью упирался бы в «сбор
// закрыт» и диалог не работал бы вовсе.
//
// Сообщение без активного обращения ничего не заводит и не сохраняется: бот
// отвечает меню. Решение владельца 2026-08-09, отменяет автозаведение от
// 2026-08-08: управление кнопками, открытый текст - только содержимое
// обращения. Случайная ссылка заводила обращение, а следующая реплика молча
// становилась его сырьём.
func (b *Bot) onItem(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		// Одно меню на пачку: пересылка десяти сообщений дала бы десять меню
		// подряд. Апдейты идут последовательно, карта без мьютекса.
		if time.Since(b.menuAt[senderID(c)]) < menuRepeat {
			return nil
		}
		b.menuAt[senderID(c)] = time.Now()
		return b.homeScreen(ctx, c, "Это сообщение я не сохраняю: сбор начинается кнопкой. "+
			"Выберите проект, нажмите «Создать тикет» и пришлите материал ещё раз.")
	}
	switch cs.Status {
	case statusInterview, statusSummary:
		return b.onAnswer(ctx, c, cs)
	case statusNormalizing, statusPublishing:
		return b.sendState(c, cs)
	}
	return b.collect(ctx, c, cs)
}

// collect кладёт сообщение сырьём в обращение. Отдельно от onItem, потому что
// сырьём приходит и слово «Меню» из своего хендлера.
func (b *Bot) collect(ctx context.Context, c tele.Context, cs *Case) error {
	askProject, err := b.cases.CollectItem(ctx, b.bot, cs, c.Message())
	reply, internal := itemReply(err, b.maxItems)
	if reply != "" {
		if sendErr := c.Send(reply); sendErr != nil {
			return sendErr
		}
	}
	// Вопрос задаётся и при неудачном приёме: строка элемента уже вставлена, и
	// на следующем сообщении признак вопроса не сработает.
	if askProject {
		if askErr := b.askProject(ctx, c, "Принял. Выберите проект, в который заводим тикет."); askErr != nil {
			return askErr
		}
	}
	if internal {
		return err
	}
	return nil
}

// itemReply - что ответить автору на неудачный приём и надо ли тащить ошибку
// наверх: известный отказ - не сбой хендлера.
func itemReply(err error, maxItems int) (text string, internal bool) {
	switch {
	case err == nil, errors.Is(err, errLimitReported):
		// Про исчерпанный лимит автору сказали один раз; повторять на каждое
		// следующее сообщение бот не должен.
		return "", false
	case errors.Is(err, ErrUnsupportedItem):
		return "Пришлите текстом, голосом или скриншотом.", false
	case errors.Is(err, ErrTooManyItems):
		return fmt.Sprintf("В обращении уже %d сообщений. Нажмите «Готово».", maxItems), false
	case errors.Is(err, ErrFileTooBig):
		return "Файл тяжелее 20 МБ, Telegram не отдаёт его боту. Пришлите скриншот или фрагмент.", false
	case errors.Is(err, ErrNotCollecting):
		return "Сбор по этому обращению закрыт, разбираю материал.", false
	}
	// Скачивание не удалось либо отказала база: элемент уже помечен failed,
	// обращение живо, и молчать нельзя - автор считает, что отправил.
	return "Не получилось принять сообщение. Пришлите его иначе.", true
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

func stateReply(status string) string {
	switch status {
	case statusCollecting:
		return "Идёт сбор обращения. Присылайте материал и нажмите «Готово»."
	case statusInterview:
		return "У вас идёт разбор обращения. Ответьте на последний вопрос - текстом " +
			"или голосовым. Если вопросов не видно, напишите, что хотели добавить, " +
			"и я спрошу заново."
	case statusSummary:
		return "Обращение ждёт вашего решения по саммари: «Публикую» или «Поправить» " +
			"под последним сообщением."
	case statusPublishing:
		return "Публикую тикет. Пришлю номер и ссылку, как только он заведётся."
	default:
		return "Разбираю обращение, минуту. Отвечу, как закончу."
	}
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
				return c.Send("Запись тяжелее 20 МБ, Telegram не отдаёт её боту. Ответьте текстом или короче.")
			}
			if sendErr := c.Send("Не получилось принять запись. Ответьте, пожалуйста, ещё раз."); sendErr != nil {
				return sendErr
			}
			return err
		}
		return c.Send("Расшифровываю ответ.")
	case strings.TrimSpace(msg.Text) != "":
		if err := b.cases.AddAnswer(ctx, cs, msg.Text); err != nil {
			if errors.Is(err, ErrNotInterview) {
				return c.Send("Обращение уже ушло дальше. Отправьте /start, чтобы завести новое.")
			}
			return err
		}
		return nil
	}
	return c.Send("Ответьте текстом или голосовым. Скриншоты принимаются только до кнопки «Готово».")
}

// onAllTrue - «Всё так»: предположения модели становятся ответом целиком.
func (b *Bot) onAllTrue(c tele.Context) error {
	if err := c.Respond(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		return c.Send("Обращение уже закрыто. Отправьте /start и заведите новое.")
	}

	round, err := strconv.Atoi(c.Data())
	if err != nil {
		return c.Send("Кнопка устарела. Ответьте, пожалуйста, текстом.")
	}

	switch err := b.cases.AcceptRound(ctx, cs, round); {
	case errors.Is(err, ErrStaleRound):
		// Кнопка прошлого раунда осталась в чате: закрывать ею текущие вопросы
		// нельзя, автор подтвердил бы не то, что видит.
		return c.Send("Это кнопка от прошлого вопроса. Ответьте на последний - текстом или голосовым.")
	case errors.Is(err, ErrNotInterview):
		return c.Send("Обращение уже ушло дальше.")
	case errors.Is(err, ErrNoSuggestion):
		return c.Send("Догадок у меня нет, подтверждать нечего. Ответьте, пожалуйста, текстом или голосовым.")
	case err != nil:
		return err
	}

	// Кнопка снимается сразу, тем же ответом: следующий ход идёт секунды, и без
	// видимой реакции человек жмёт её ещё раз. Каждое лишнее нажатие - ещё один
	// ответ в истории и ещё один ход модели поверх незаконченного.
	if err := c.Edit(acceptedText(c)); err != nil {
		b.log.Warn("round_edit_failed", "user_id", senderID(c), "error", err)
		return c.Send("Принял. Уточняю дальше.")
	}
	return nil
}

// acceptedText помечает раунд принятым прямо в исходном сообщении: история
// переписки остаётся читаемой, а кнопки под ним больше нет.
func acceptedText(c tele.Context) string {
	text := "Раунд вопросов"
	if msg := c.Message(); msg != nil && msg.Text != "" {
		text = msg.Text
	}
	return text + "\n\n---\nПринято: всё так. Уточняю дальше."
}

// onPublish - «Публикую»: подтверждение саммари, после которого тикет уходит в
// GitHub, а файлы обращения удаляются.
func (b *Bot) onPublish(c tele.Context) error {
	if err := c.Respond(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		return c.Send("Обращение уже закрыто. Отправьте /start и заведите новое.")
	}

	if err := b.cases.ConfirmSummary(ctx, cs); err != nil {
		if errors.Is(err, ErrNoSummary) {
			// Кнопка осталась в чате, а обращение ушло дальше - или это уже
			// другое обращение того же автора: слот один, кнопка вечная.
			switch cs.Status {
			case statusInterview:
				return c.Send("Переписываю саммари с вашей правкой. Покажу заново - тогда и опубликуем.")
			case statusPublishing:
				return c.Send("Уже публикую. Пришлю номер, как только тикет заведётся.")
			}
			return b.sendState(c, cs)
		}
		return err
	}
	return c.Send("Публикую. Пришлю номер и ссылку.")
}

// onFix - «Поправить». Состояние не меняет: правкой становится следующее
// сообщение автора, и обрабатывает его тот же onAnswer. Отдельное состояние
// «ждём замечание» дало бы восьмой статус ради одной подсказки.
//
// Гвардия обязательна: кнопка живёт в переписке дольше обращения, и без неё
// приглашение «напишите правку» отправило бы следующий текст автора в сырьё
// нового обращения или в пустоту.
func (b *Bot) onFix(c tele.Context) error {
	if err := c.Respond(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		return c.Send("Обращение уже закрыто. Нажмите «Меню» и начните новое.")
	}
	if !inDialog(cs.Status) {
		return b.sendState(c, cs)
	}
	return c.Send("Что поправить? Напишите или наговорите - перепишу и покажу снова.")
}

func (b *Bot) onDone(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		return c.Send("Сейчас нечего завершать. Нажмите «Меню» и начните с проекта.")
	}
	// Слово «Готово» в разговоре - обычный ответ («статус заказа - Готово»), а
	// не команда сбора: reply-кнопка маршрутизируется по точному тексту раньше
	// OnText, и без этой развилки ответ автора пропадал бы молча.
	if !isCommand(c) && inDialog(cs.Status) {
		return b.onAnswer(ctx, c, cs)
	}

	switch err := b.cases.FinishCollect(ctx, cs); {
	case errors.Is(err, ErrNoItems):
		return c.Send("Пока нечего разбирать. Пришлите текст, голосовое или скриншот.")
	case errors.Is(err, ErrNoProject):
		// Сырьё на месте, статус не менялся: не хватает только проекта.
		return b.askProject(ctx, c, "Выберите проект, без него тикет некуда заводить.")
	case errors.Is(err, ErrNotCollecting):
		// Статусный ответ вместо «разбираю»: /done в нормализации и правда
		// значит «идёт разбор», а в разговоре бот ждёт автора, а не наоборот.
		return b.sendState(c, cs)
	case err != nil:
		return err
	}
	return c.Send("Разбираю материал. Пришлю протокол, как закончу.", homeKeyboard())
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
		return b.homeScreen(ctx, c, "Начнём.")
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
		return b.homeScreen(ctx, c, "Сбрасывать нечего.")
	}
	if cs.Status == statusPublishing {
		// Работа publish уже в очереди: погасив обращение здесь, мы получили
		// бы issue по отменённому.
		return c.Send("Тикет уже уходит в GitHub, отменить не получится. Пришлю номер.")
	}

	material, err := b.cases.HasMaterial(ctx, cs.ID)
	if err != nil {
		return err
	}
	if material {
		// Подтверждение привязано к обращению: кнопка живёт в переписке дольше,
		// чем слот, и без привязки отменяла бы уже другое обращение автора.
		markup := &tele.ReplyMarkup{}
		markup.Inline(
			markup.Row(markup.Data("Да, сбросить", resetYesBtn.Unique, cs.ID)),
			markup.Row(markup.Data("Оставить", resetNoBtn.Unique)),
		)
		return c.Send("Обращение будет отменено безвозвратно, вместе с файлами. Сбросить?", markup)
	}
	if err := b.cases.CancelCase(ctx, cs, "reset"); err != nil {
		return err
	}
	return b.homeScreen(ctx, c, "Сброшено.")
}

// onResetYes - подтверждение сброса. Кнопка живёт в переписке дольше экрана:
// обращение могло уйти дальше или закрыться, отменяется то, что активно сейчас.
func (b *Bot) onResetYes(c tele.Context) error {
	if err := c.Respond(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		return c.Edit("Обращение уже закрыто.")
	}
	if c.Data() != cs.ID {
		return c.Edit("Кнопка устарела: это подтверждение другого обращения. " +
			"Чтобы сбросить текущее, нажмите «Сброс».")
	}
	if err := b.cases.CancelCase(ctx, cs, "reset"); err != nil {
		if errors.Is(err, ErrPublishing) {
			return c.Edit("Тикет уже уходит в GitHub, отменить не получится. Пришлю номер.")
		}
		return err
	}
	if err := c.Edit("Обращение отменено, файлы удалены."); err != nil {
		b.log.Warn("reset_edit_failed", "user_id", senderID(c), "error", err)
	}
	return b.homeScreen(ctx, c, "Начнём заново.")
}

func (b *Bot) onResetNo(c tele.Context) error {
	if err := c.Respond(); err != nil {
		return err
	}
	// Правка вместо нового сообщения: кнопки подтверждения снимаются, чтобы
	// «Да, сбросить» не сработала неделю спустя.
	return c.Edit("Оставил, продолжаем.")
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

// roundKeyboard - «Всё так» под раундом вопросов. Номер раунда уезжает в
// callback_data: кнопки прошлых раундов остаются в переписке, и по нажатию надо
// понять, к каким вопросам оно относится.
func roundKeyboard(round int) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	markup.Inline(markup.Row(markup.Data("Всё так", allTrueBtn.Unique, strconv.Itoa(round))))
	return markup
}

// summaryKeyboard - решение по саммари. Отмены здесь нет: её несёт «Сброс» на
// панели, и один путь выхода лучше двух одинаковых.
func summaryKeyboard() *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	markup.Inline(markup.Row(
		markup.Data("Публикую", publishBtn.Unique),
		markup.Data("Поправить", fixBtn.Unique),
	))
	return markup
}

// onTicketList - вход в просмотр командой. Нужен отдельно от кнопки меню: меню
// действий рисуется только автору без активного обращения, а вспоминает про
// старый тикет человек как раз посреди интервью.
func (b *Bot) onTicketList(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()
	return b.askProjectFor(ctx, c, "Тикеты какого проекта показать?", ticketsBtn)
}

// onTickets - список тикетов проекта.
func (b *Bot) onTickets(c tele.Context) error {
	if err := c.Respond(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	project, ok, err := b.project(ctx, c.Data())
	if err != nil {
		return err
	}
	if !ok {
		return c.Send("Проект недоступен. Отправьте /start и выберите заново.")
	}

	tickets, err := b.tickets.List(ctx, project)
	if err != nil {
		return err
	}
	if len(tickets) == 0 {
		return c.Send("По проекту «" + project.Title + "» тикетов ещё нет.")
	}
	b.log.Info("tickets_listed", "user_id", senderID(c), "project", project.Slug, "count", len(tickets))

	markup := &tele.ReplyMarkup{}
	rows := make([]tele.Row, 0, len(tickets))
	var text strings.Builder
	text.WriteString("Тикеты проекта «" + project.Title + "»:\n\n")
	for _, t := range tickets {
		text.WriteString(ticketLine(t) + "\n")
		rows = append(rows, markup.Row(markup.Data(
			fmt.Sprintf("#%d %s", t.Number, statusTitle(t.Status)),
			cardBtn.Unique, cardData(project.Slug, t.Number))))
	}
	markup.Inline(rows...)
	return b.sendLong(c.Recipient(), text.String(), markup)
}

// onCard - карточка тикета. Кнопка отмены только автору: просмотр общий, отмена
// нет.
func (b *Bot) onCard(c tele.Context) error {
	if err := c.Respond(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	project, number, ok, err := b.cardTarget(ctx, c)
	if err != nil || !ok {
		return err
	}

	ticket, err := b.tickets.Load(ctx, project, number)
	switch {
	case errors.Is(err, ErrIssueGone):
		return c.Send(fmt.Sprintf("Тикета #%d больше нет в GitHub.", number))
	case err != nil:
		return err
	case ticket == nil:
		return c.Send("Тикет не найден. Откройте список заново.")
	}
	b.log.Info("ticket_opened", "user_id", senderID(c), "case_id", ticket.CaseID, "issue", number)

	markup := &tele.ReplyMarkup{}
	var rows []tele.Row
	if ticket.UserID == senderID(c) && !ticket.Status.Final {
		rows = append(rows, markup.Row(markup.Data("Отменить тикет", killBtn.Unique,
			cardData(project.Slug, number))))
	}
	rows = append(rows, markup.Row(markup.Data("К списку", ticketsBtn.Unique, project.Slug)))
	markup.Inline(rows...)
	return b.sendLong(c.Recipient(), cardText(ticket), markup)
}

// onKill ставит работу отмены. Ответ автору синхронный, сама отмена идёт
// очередью: это мутация в GitHub, она обязана пережить рестарт.
func (b *Bot) onKill(c tele.Context) error {
	if err := c.Respond(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	project, number, ok, err := b.cardTarget(ctx, c)
	if err != nil || !ok {
		return err
	}

	err = b.tickets.Cancel(ctx, project, number, senderID(c))
	switch {
	case errors.Is(err, ErrNotAuthor):
		b.log.Warn("cancel_denied", "user_id", senderID(c), "project", project.Slug, "issue", number)
		return c.Send("Отменить тикет может только его автор.")
	case errors.Is(err, ErrIssueGone):
		return c.Send("Тикет не найден. Откройте список заново.")
	case err != nil:
		return err
	}
	return c.Send(fmt.Sprintf("Отменяю тикет #%d, сообщу когда закроется.", number))
}

// cardTarget разбирает кнопку карточки. Второе значение false означает, что
// автору уже ответили отказом.
func (b *Bot) cardTarget(ctx context.Context, c tele.Context) (Project, int, bool, error) {
	slug, raw, found := strings.Cut(c.Data(), ":")
	number, err := strconv.Atoi(raw)
	if !found || err != nil {
		b.log.Warn("bad_card_data", "user_id", senderID(c), "data", c.Data())
		return Project{}, 0, false, c.Send("Кнопка устарела. Откройте список заново.")
	}

	project, ok, err := b.project(ctx, slug)
	if err != nil {
		return Project{}, 0, false, err
	}
	if !ok {
		return Project{}, 0, false, c.Send("Проект недоступен. Отправьте /start и выберите заново.")
	}
	return project, number, true, nil
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

func ticketLine(t Ticket) string {
	return fmt.Sprintf("#%d - %s - %s", t.Number, statusTitle(t.Status), t.Title)
}

// statusTitle - что показать вместо статуса, когда GitHub не ответил. Пустая
// строка выглядела бы как «статус неизвестен сервису», а он просто недоступен.
func statusTitle(s Status) string {
	if s.Title == "" {
		return "статус недоступен"
	}
	return s.Title
}

func cardText(t *Ticket) string {
	var text strings.Builder
	fmt.Fprintf(&text, "Тикет #%d - %s\n%s\n\nАвтор: %s\n", t.Number, statusTitle(t.Status), t.Title, t.Author)
	if t.Body != "" {
		text.WriteString("\n" + t.Body + "\n")
	}
	if t.Comment != "" {
		text.WriteString("\nПоследний комментарий:\n" + t.Comment + "\n")
	}
	if t.URL != "" {
		text.WriteString("\n" + t.URL)
	}
	return text.String()
}

// projectHelp - подсказка. Один пример важнее описания синтаксиса: ссылку
// копируют со страницы репозитория и присылают как есть.
const projectHelp = "Пришлите ссылку на репозиторий проекта:\n" +
	"https://github.com/owner/repo\n\n" +
	"Название и описание я соберу сам по README. Можно задать их явно:\n" +
	"owner/repo Название | Чем занят сервис"

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
		return err
	}
	return nil
}

// onAddProject - кнопка «Добавить проект»: следующее сообщение автора - ссылка.
// При активном обращении кнопка не работает: ссылка ушла бы в него сырьём.
func (b *Bot) onAddProject(c tele.Context) error {
	if err := c.Respond(); err != nil {
		return err
	}

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
	return c.Send("Пришлите ссылку на репозиторий следующим сообщением.")
}

// onProjectLink - сообщение, обещанное после «Добавить проект». Зовёт allow:
// хендлера у него нет, ожидание разбирается до маршрутизации.
func (b *Bot) onProjectLink(c tele.Context) error {
	err := b.addProject(c, strings.TrimSpace(c.Text()))
	if errors.Is(err, ErrBadProjectRef) {
		// Обещание ссылки продлевается: автор ошибся, а не передумал. Только
		// здесь: команда /project с мусором ждать следующее сообщение не
		// обещала, и взведённое ею ожидание крало бы сырьё активного сбора.
		b.awaitLink[senderID(c)] = time.Now()
		return c.Send(projectHelp)
	}
	return err
}

// addProject заводит проект и отвечает карточкой. ErrBadProjectRef возвращает
// как есть: подсказку каждый вход даёт свою.
func (b *Bot) addProject(c tele.Context, args string) error {
	// Бюджет с запасом: чтение репозитория, README и ход модели.
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	project, source, err := b.projects.Add(ctx, senderID(c), args)
	switch {
	case errors.Is(err, ErrBadProjectRef):
		return err
	case errors.Is(err, ErrSlugTaken):
		return c.Send("Проект с таким именем уже заведён на другой репозиторий. " +
			"Напишите владельцу сервиса - выключить проект из бота пока можно только через базу.")
	case err != nil:
		b.log.Warn("project_add_failed", "user_id", senderID(c), "error", err)
		return c.Send(projectFailText(err))
	}

	return b.sendLong(c.Recipient(), projectCard(project, source))
}

// projectCard показывает и то, откуда взялось описание: контекст уходит в
// инструкцию интервью и определяет вопросы по всем будущим обращениям проекта,
// поэтому придуманное моделью автор должен отличать от своего.
func projectCard(p ProjectConfig, source string) string {
	return fmt.Sprintf("Проект «%s» заведён и появился в меню.\nРепозиторий: %s/%s\n\n%s\n\n%s\n"+
		"Переписать: /project %s/%s Название | Описание",
		p.Title, p.Owner, p.Repo, p.Context, projectSourceNote(source), p.Owner, p.Repo)
}

func projectSourceNote(source string) string {
	switch source {
	case "модель":
		return "Название и описание я собрал по README - проверьте, так ли это."
	case "репозиторий":
		return "Описание модель собрать не смогла, взял из полей репозитория."
	default:
		return "Название и описание ваши, я их не трогал."
	}
}

// projectFailText объясняет отказ словами автора, а не статусом API: для
// fine-grained токена «нет репозитория» и «нет доступа» неразличимы.
func projectFailText(err error) string {
	var apiErr *githubError
	if errors.As(err, &apiErr) && (apiErr.status == http.StatusNotFound || apiErr.status == http.StatusForbidden) {
		return "Репозиторий недоступен: токен сервиса его не видит либо не может писать в Issues. " +
			"Проверьте адрес и права токена, потом повторите."
	}
	return "Не получилось завести проект. Попробуйте ещё раз, а если повторится - напишите владельцу сервиса."
}
