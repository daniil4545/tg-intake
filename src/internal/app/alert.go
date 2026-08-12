package app

import (
	"fmt"

	tele "gopkg.in/telebot.v4"
)

// Уведомление владельцу: короткая шапка о движении тикета в отдельный чат,
// куда пишут и алерты контура. Адресат - чат из конфига, а не роль в сервисе:
// кто читает ленту, решается составом чата.

// newAlertBot собирает отправителя уведомлений. Пустой чат - уведомлений нет,
// пустой токен - пишем ботом сервиса (он добавлен в общую группу).
//
// Offline пропускает getMe: канал уведомлений не имеет права уронить приём
// обращений, и отозванный чужой токен обязан ломать уведомление, а не старт.
// Цена - неверный токен виден первой отправкой, в логе и в job-errors. Отказ
// сборки зовущий тоже не считает фатальным, поэтому nil - рабочий ответ.
func newAlertBot(cfg Config, main *tele.Bot) (*tele.Bot, error) {
	if cfg.AlertChatID == 0 {
		return nil, nil
	}
	if cfg.AlertBotToken == "" {
		return main, nil
	}
	tb, err := tele.NewBot(tele.Settings{
		Token:       cfg.AlertBotToken,
		Client:      proxyClient(cfg.TelegramProxy),
		Offline:     true,
		Synchronous: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create alert bot: %w", err)
	}
	return tb, nil
}

func alertPublished(p Project, cs *Case, author User, number int, url string) string {
	text := alertMessage("Новый тикет", p, cs, author, number, url)
	if cs.Incomplete {
		text += "\nКонтракт недобран: тикет помечен incomplete."
	}
	return text
}

func alertCancelled(p Project, cs *Case, author User, number int, url string) string {
	return alertMessage("Тикет отменён автором", p, cs, author, number, url)
}

func alertMessage(head string, p Project, cs *Case, author User, number int, url string) string {
	return fmt.Sprintf("%s: %s\n%s\nАвтор: %s\n#%d %s",
		head, p.Slug, cs.Title, authorName(author), number, url)
}
