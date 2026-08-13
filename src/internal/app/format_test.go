package app

import (
	"errors"
	"strings"
	"testing"

	tele "gopkg.in/telebot.v4"
)

// TestItemReply: ответ на приём сырья говорит автору, что делать дальше, и
// молчит только там, где сказать уже нечего. Молчание не по делу читается как
// «бот принял», и материал теряется незаметно.
func TestItemReply(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		want     string
		internal bool
	}{
		{"приняли", nil, "", false},
		{"лимит уже назван", errLimitReported, "", false},
		{"чужой формат", ErrUnsupportedItem, "Пришлите текстом", false},
		{"лимит исчерпан", ErrTooManyItems, "уже 30 сообщений", false},
		{"файл тяжёлый", ErrFileTooBig, "20 МБ", false},
		{"сбор закрыт", ErrNotCollecting, "разбираю материал", false},
		{"отказ базы", errors.New("db is down"), "Пришлите его иначе", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, internal := itemReply(c.err, 30)
			if c.want == "" && text != "" {
				t.Fatalf("ожидалось молчание, получено %q", text)
			}
			if !strings.Contains(text, c.want) {
				t.Errorf("ответ %q не содержит %q", text, c.want)
			}
			if internal != c.internal {
				t.Errorf("признак внутренней ошибки: %v, ожидался %v", internal, c.internal)
			}
		})
	}
}

// TestStateReply: на каждом состоянии автор узнаёт, чего ждёт бот. Пустой или
// одинаковый ответ оставлял бы человека в тупике - активное обращение одно, и
// упереться в него можно из любого места.
func TestStateReply(t *testing.T) {
	seen := map[string]bool{}
	for _, status := range []string{statusCollecting, statusInterview, statusSummary,
		statusPublishing, statusNormalizing} {
		reply := stateReply(status)
		if reply == "" {
			t.Fatalf("состояние %s осталось без ответа", status)
		}
		if seen[reply] && status != statusNormalizing {
			t.Errorf("состояние %s отвечает чужим текстом: %q", status, reply)
		}
		seen[reply] = true
	}
	if !strings.Contains(stateReply(statusCollecting), "«Готово»") {
		t.Error("в сборе не сказано, чем его закончить")
	}
}

// TestCardHidesCancelWithoutStatus: карточка не выдаёт незнание за факт. При
// молчащем GitHub статус не прочитан, и «не доигран» - это незнание: отмену
// предлагать нельзя, тикет мог быть закрыт неделю назад.
func TestCardHidesCancelWithoutStatus(t *testing.T) {
	ticket := Ticket{Number: 42, Title: "Форма не сохраняется", Author: "Иван",
		Unavailable: true}

	if !strings.Contains(cardText(&ticket), "статус недоступен") {
		t.Errorf("карточка молчит о недоступном статусе:\n%s", cardText(&ticket))
	}
	if cancelOffered(&ticket, ticket.UserID) {
		t.Error("отмена предложена по непрочитанному статусу")
	}

	ticket.Unavailable = false
	ticket.Status = Status{Title: "в работе"}
	if !cancelOffered(&ticket, ticket.UserID) {
		t.Error("отмена не предложена автору живого тикета")
	}

	ticket.Status = Status{Title: "закрыт", Final: true}
	if cancelOffered(&ticket, ticket.UserID) {
		t.Error("отмена предложена по закрытому тикету")
	}
	if cancelOffered(&Ticket{UserID: 1}, 2) {
		t.Error("отмена предложена не автору")
	}
}

// TestTicketLine: строка списка несёт номер, статус и заголовок - по ней автор
// узнаёт свой тикет, не открывая карточку.
func TestTicketLine(t *testing.T) {
	line := ticketLine(Ticket{Number: 7, Title: "Не грузится карточка"})
	for _, want := range []string{"#7", "статус недоступен", "Не грузится карточка"} {
		if !strings.Contains(line, want) {
			t.Errorf("в строке списка нет %q: %q", want, line)
		}
	}
}

// TestInBatch: молчание достаётся пачке, а не человеку. Пересылки и альбомы
// приходят десятками за секунды, набранное руками сообщение - по одному, и
// тишина в ответ на него читается как «бот принял».
func TestInBatch(t *testing.T) {
	if inBatch(&tele.Message{Text: "заявка не сохраняется"}) {
		t.Error("набранное сообщение принято за пачку")
	}
	if !inBatch(&tele.Message{AlbumID: "42"}) {
		t.Error("альбом не считается пачкой")
	}
	if !inBatch(&tele.Message{OriginalSender: &tele.User{ID: 5}}) {
		t.Error("пересылка не считается пачкой")
	}
	if inBatch(nil) {
		t.Error("пустое сообщение принято за пачку")
	}
}
