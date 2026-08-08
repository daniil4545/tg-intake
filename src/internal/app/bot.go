package app

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tele "gopkg.in/telebot.v4"
)

const dbTimeout = 10 * time.Second

// projectBtn: telebot маршрутизирует callback по Unique и кодирует кнопку как
// \f<unique>|<data>. Свой формат callback_data мимо Unique хендлером не
// поймается.
var projectBtn = &tele.Btn{Unique: "project"}

type Bot struct {
	bot     *tele.Bot
	pool    *pgxpool.Pool
	log     *slog.Logger
	allowed []int64
}

func NewBot(cfg Config, pool *pgxpool.Pool, log *slog.Logger) (*Bot, error) {
	b := &Bot{pool: pool, log: log, allowed: cfg.AllowedIDs}

	// Verbose дампит сырые payload Bot API вместе с текстами сообщений, а
	// стандартный OnError пишет через stdlib log мимо JSON.
	tb, err := tele.NewBot(tele.Settings{
		Token:   cfg.BotToken,
		Poller:  &tele.LongPoller{Timeout: 10 * time.Second},
		Verbose: false,
		OnError: func(err error, c tele.Context) {
			log.Error("handler_failed", "error", err, "user_id", senderID(c))
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create bot: %w", err)
	}

	tb.Use(b.allow)
	tb.Handle("/start", b.onStart)
	tb.Handle(projectBtn, b.onProject)

	b.bot = tb
	return b, nil
}

func (b *Bot) Start() { b.bot.Start() }

func (b *Bot) Stop() { b.bot.Stop() }

// allow - единственная точка контроля доступа: обойти её хендлером нельзя.
func (b *Bot) allow(next tele.HandlerFunc) tele.HandlerFunc {
	return func(c tele.Context) error {
		sender := c.Sender()
		if sender == nil {
			b.log.Warn("update_without_sender")
			return nil
		}
		if chat := c.Chat(); chat == nil || chat.Type != tele.ChatPrivate {
			// В M1 сюда придут скриншоты и саммари обращения: групповой чат
			// для них не место.
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

func (b *Bot) onStart(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	defer cancel()

	sender := c.Sender()
	user := User{ID: sender.ID, First: sender.FirstName, Last: sender.LastName, Username: sender.Username}
	if err := UpsertUser(ctx, b.pool, user); err != nil {
		return err
	}

	projects, err := ListProjects(ctx, b.pool)
	if err != nil {
		return err
	}
	if len(projects) == 0 {
		b.log.Warn("start_no_projects", "user_id", sender.ID)
		return c.Send("Проекты ещё не заведены. Напишите владельцу сервиса.")
	}

	markup := &tele.ReplyMarkup{}
	rows := make([]tele.Row, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, markup.Row(markup.Data(p.Title, projectBtn.Unique, p.Slug)))
	}
	markup.Inline(rows...)

	b.log.Info("start", "user_id", sender.ID, "projects", len(projects))
	return c.Send("Выберите проект.", markup)
}

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
		b.log.Warn("unknown_project", "user_id", c.Sender().ID, "slug", slug)
		return c.Send("Проект недоступен. Отправьте /start и выберите заново.")
	}

	b.log.Info("project_selected", "user_id", c.Sender().ID, "slug", slug)
	return c.Send("Проект «" + projects[index].Title + "» выбран. Приём обращений появится в следующем срезе.")
}
