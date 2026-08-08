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
)

// Инлайн-кнопки: telebot маршрутизирует callback по Unique и кодирует кнопку
// как \f<unique>|<data>. Свой формат callback_data мимо Unique хендлером не
// поймается. Кнопки сбора - reply-клавиатура, они маршрутизируются по тексту,
// поэтому один и тот же Btn идёт и в Handle, и в клавиатуру.
var (
	projectBtn  = &tele.Btn{Unique: "project"}
	createBtn   = &tele.Btn{Unique: "create"}
	continueBtn = &tele.Btn{Unique: "continue"}
	restartBtn  = &tele.Btn{Unique: "restart"}
	allTrueBtn  = &tele.Btn{Unique: "all_true"}
	publishBtn  = &tele.Btn{Unique: "publish"}
	fixBtn      = &tele.Btn{Unique: "fix"}
	dropBtn     = &tele.Btn{Unique: "drop"}
	doneBtn     = &tele.Btn{Text: "Готово"}
	cancelBtn   = &tele.Btn{Text: "Отмена"}
)

type Bot struct {
	bot      *tele.Bot
	pool     *pgxpool.Pool
	cases    *Cases
	log      *slog.Logger
	allowed  []int64
	maxItems int
}

func NewBot(ctx context.Context, cfg Config, pool *pgxpool.Pool, cases *Cases, log *slog.Logger) (*Bot, error) {
	b := &Bot{pool: pool, cases: cases, log: log, allowed: cfg.AllowedIDs, maxItems: cfg.MaxItems}

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
	tb.Handle("/cancel", b.onCancel)

	tb.Handle(projectBtn, b.onProject)
	tb.Handle(createBtn, b.onCreate)
	tb.Handle(continueBtn, b.onContinue)
	tb.Handle(restartBtn, b.onRestart)
	tb.Handle(allTrueBtn, b.onAllTrue)
	tb.Handle(publishBtn, b.onPublish)
	tb.Handle(fixBtn, b.onFix)
	tb.Handle(dropBtn, b.onCancel)
	tb.Handle(doneBtn, b.onDone)
	tb.Handle(cancelBtn, b.onCancel)

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
		if _, err := b.bot.Send(to, strings.TrimRight(text[:cut], "\n"), opts...); err != nil {
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
		return next(c)
	}
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

	// Активное обращение вне сбора занимает единственный слот автора, и меню
	// проектов из него ведёт в тупик: выбор проекта упрётся в «сбор закрыт», а
	// человек читает это как зависший бот. Говорим прямо, где он находится.
	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs != nil && cs.Status != statusCollecting {
		return b.sendState(c, cs)
	}

	b.log.Info("start", "user_id", senderID(c))
	return b.askProject(ctx, c, "Выберите проект.")
}

// askProject показывает меню проектов. Вопрос задаётся ровно один раз за
// обращение (признак приходит из CollectItem) и повторяется на «Готово» без
// проекта: тикет некуда заводить.
func (b *Bot) askProject(ctx context.Context, c tele.Context, text string) error {
	projects, err := ListProjects(ctx, b.pool)
	if err != nil {
		return err
	}
	if len(projects) == 0 {
		b.log.Warn("no_projects", "user_id", senderID(c))
		return c.Send("Проекты ещё не заведены. Напишите владельцу сервиса.")
	}

	markup := &tele.ReplyMarkup{}
	rows := make([]tele.Row, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, markup.Row(markup.Data(p.Title, projectBtn.Unique, p.Slug)))
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
		markup.Inline(markup.Row(markup.Data("Создать тикет", createBtn.Unique, slug)))
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
		// продолжать нечего, там обращение двигает ответ автора.
		if cs.Status != statusCollecting {
			return b.sendState(c, cs)
		}
		markup := &tele.ReplyMarkup{}
		markup.Inline(markup.Row(
			markup.Data("Продолжить", continueBtn.Unique, slug),
			markup.Data("Начать заново", restartBtn.Unique, slug),
		))
		return c.Send("У вас уже есть незакрытое обращение.", markup)
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

func (b *Bot) onRestart(c tele.Context) error {
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
		if err := b.cases.CancelCase(ctx, cs, "restart"); err != nil {
			if errors.Is(err, ErrPublishing) {
				return c.Send("Прошлое обращение уже уходит в GitHub. Заведём новое, как только придёт номер.")
			}
			return err
		}
	}
	if _, _, err := b.cases.StartCase(ctx, author(c), c.Data()); err != nil {
		return err
	}
	return c.Send("Начали заново. Присылайте материал и нажмите «Готово».", collectKeyboard())
}

// onItem принимает любое сообщение автора. Куда оно пойдёт, решает состояние
// обращения, а не то, какой экран автор видел последним: сырьё в сборе, ответ в
// разговоре. Без этой развилки ответ на вопрос интервью упирался бы в «сбор
// закрыт» и диалог не работал бы вовсе.
//
// Активного обращения нет - оно заводится тут же, сырьё не теряется: человек
// начинает с материала, а не с репозитория.
func (b *Bot) onItem(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs != nil {
		switch cs.Status {
		case statusInterview, statusSummary:
			return b.onAnswer(ctx, c, cs)
		case statusNormalizing, statusPublishing:
			return b.sendState(c, cs)
		}
	}
	if cs == nil {
		cs, _, err = b.cases.StartCase(ctx, author(c), "")
		if err != nil {
			return err
		}
		// Кнопки сбора видны с самого начала: меню проекта несёт собственную
		// разметку, а клавиатура «Готово»/«Отмена» - reply, и в одно сообщение
		// с ним не помещается.
		if err := c.Send("Собираю новое обращение. Когда закончите, нажмите «Готово».", collectKeyboard()); err != nil {
			return err
		}
	}

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
// зависший бот.
//
// Кнопка сброса идёт тем же сообщением: команду /cancel в переписке ещё надо
// вспомнить, а выход нужен ровно в тот момент, когда человек уткнулся.
func (b *Bot) sendState(c tele.Context, cs *Case) error {
	// Из публикации выхода нет: работа уже в очереди, и отмена разошлась бы с
	// ответом GitHub.
	if cs.Status == statusPublishing {
		return c.Send(stateReply(cs.Status))
	}

	markup := &tele.ReplyMarkup{}
	markup.Inline(markup.Row(markup.Data("Отменить обращение", dropBtn.Unique)))
	return c.Send(stateReply(cs.Status), markup)
}

func stateReply(status string) string {
	switch status {
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
			// Кнопка осталась в чате, а обращение ушло дальше: либо публикация
			// уже идёт, либо автор прислал правку и саммари переписывается.
			if cs.Status == statusInterview {
				return c.Send("Переписываю саммари с вашей правкой. Покажу заново - тогда и опубликуем.")
			}
			return c.Send("Уже публикую. Пришлю номер, как только тикет заведётся.")
		}
		return err
	}
	return c.Send("Публикую. Пришлю номер и ссылку.")
}

// onFix - «Поправить». Состояние не меняет: правкой становится следующее
// сообщение автора, и обрабатывает его тот же onAnswer. Отдельное состояние
// «ждём замечание» дало бы восьмой статус ради одной подсказки.
func (b *Bot) onFix(c tele.Context) error {
	if err := c.Respond(); err != nil {
		return err
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
		return c.Send("Сейчас нечего завершать. Отправьте /start или просто пришлите материал.")
	}

	switch err := b.cases.FinishCollect(ctx, cs); {
	case errors.Is(err, ErrNoItems):
		return c.Send("Пока нечего разбирать. Пришлите текст, голосовое или скриншот.")
	case errors.Is(err, ErrNoProject):
		// Сырьё на месте, статус не менялся: не хватает только проекта.
		return b.askProject(ctx, c, "Выберите проект, без него тикет некуда заводить.")
	case errors.Is(err, ErrNotCollecting):
		return c.Send("Сбор уже закрыт, разбираю материал.")
	case err != nil:
		return err
	}
	return c.Send("Разбираю материал. Пришлю протокол, как закончу.", hideKeyboard())
}

// onCancel - «Отмена» из клавиатуры сбора, кнопка под саммари и /cancel: один
// путь на все три входа.
func (b *Bot) onCancel(c tele.Context) error {
	// Отмена приходит и кнопкой под саммари: без Respond она крутится до
	// таймаута Telegram.
	if c.Callback() != nil {
		if err := c.Respond(); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	cs, err := b.cases.Active(ctx, senderID(c))
	if err != nil {
		return err
	}
	if cs == nil {
		return c.Send("Отменять нечего.", hideKeyboard())
	}
	if err := b.cases.CancelCase(ctx, cs, "user"); err != nil {
		if errors.Is(err, ErrPublishing) {
			// Работа publish уже в очереди: погасив обращение здесь, мы получили
			// бы issue по отменённому.
			return c.Send("Тикет уже уходит в GitHub, отменить не получится. Пришлю номер.")
		}
		return err
	}
	return c.Send("Обращение отменено, файлы удалены.", hideKeyboard())
}

// collectKeyboard - кнопки сбора. Reply-клавиатура, а не инлайн: её видно
// всегда и не надо искать кнопку выше по переписке.
func collectKeyboard() *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{ResizeKeyboard: true, IsPersistent: true}
	markup.Reply(markup.Row(*doneBtn, *cancelBtn))
	return markup
}

// hideKeyboard убирает кнопки сбора: обращение закрыто, нажимать «Готово»
// больше не по чему.
func hideKeyboard() *tele.ReplyMarkup {
	return &tele.ReplyMarkup{RemoveKeyboard: true}
}

// roundKeyboard - «Всё так» под раундом вопросов. Номер раунда уезжает в
// callback_data: кнопки прошлых раундов остаются в переписке, и по нажатию надо
// понять, к каким вопросам оно относится.
func roundKeyboard(round int) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	markup.Inline(markup.Row(markup.Data("Всё так", allTrueBtn.Unique, strconv.Itoa(round))))
	return markup
}

func summaryKeyboard() *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	markup.Inline(markup.Row(
		markup.Data("Публикую", publishBtn.Unique),
		markup.Data("Поправить", fixBtn.Unique),
		markup.Data("Отмена", dropBtn.Unique),
	))
	return markup
}
