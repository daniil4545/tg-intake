package app

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

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
		statusPublishing, statusAnswering, statusNormalizing} {
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

// TestProjectMenu: у проекта два входа в разговор - тикет и вопрос, плюс
// просмотр тикетов. Без кнопки «Спросить» режим вопроса недостижим.
func TestProjectMenu(t *testing.T) {
	want := map[string]string{
		"Создать тикет":     createBtn.Unique,
		"Спросить":          askBtn.Unique,
		"Посмотреть тикеты": ticketsBtn.Unique,
	}
	for _, row := range projectMenu("tg-intake").InlineKeyboard {
		for _, btn := range row {
			unique, ok := want[btn.Text]
			if !ok {
				continue
			}
			if btn.Unique != unique || btn.Data != "tg-intake" {
				t.Errorf("кнопка %q ведёт в %q с данными %q", btn.Text, btn.Unique, btn.Data)
			}
			delete(want, btn.Text)
		}
	}
	for text := range want {
		t.Errorf("в меню проекта нет кнопки %q", text)
	}
}

// TestAnswerKeyboard: под ответом из документации два выхода - завести тикет и
// закончить разговор. Без них автор остаётся с занятым слотом и без пути к
// правке.
func TestAnswerKeyboard(t *testing.T) {
	seen := map[string]string{}
	for _, row := range answerKeyboard().InlineKeyboard {
		for _, btn := range row {
			seen[btn.Unique] = btn.Text
		}
	}
	if seen[toTicketBtn.Unique] != "Создать тикет" {
		t.Errorf("кнопка перехода в тикет: %q", seen[toTicketBtn.Unique])
	}
	if seen[endAskBtn.Unique] != "Закончить разговор" {
		t.Errorf("кнопка конца разговора: %q", seen[endAskBtn.Unique])
	}
}

// TestProjectAsk: в режиме вопроса бот не обещает тикет - тикета там не будет,
// а обещание автор считает фактом.
func TestProjectAsk(t *testing.T) {
	ask := projectAsk(&Case{Mode: modeAsk})
	if strings.Contains(ask, "тикет") {
		t.Errorf("вопрос о проекте обещает тикет: %q", ask)
	}
	if !strings.Contains(projectAsk(&Case{Mode: modeTicket}), "тикет") {
		t.Error("в режиме тикета не сказано, зачем нужен проект")
	}
}

// TestContinueText: экран продолжения называет жанр живого разговора и выход в
// нужный автору. Разговор по документации висит в сборе до «Закончить разговор»,
// и безымянное «Продолжить» увело бы пришедшего за тикетом обратно в вопрос.
func TestContinueText(t *testing.T) {
	live := continueText(modeAsk, modeTicket)
	if !strings.Contains(live, "документации") || !strings.Contains(live, "тикет") {
		t.Errorf("экран не назвал ни режим, ни выход: %q", live)
	}
	if !strings.Contains(continueText(modeTicket, modeAsk), "тикета") {
		t.Errorf("экран не назвал режим живого обращения: %q", continueText(modeTicket, modeAsk))
	}
}

// TestCardFitsOneMessage: карточка помещается в одно сообщение даже когда
// комментарий разработчика - многостраничный разбор. Иначе Telegram получает
// карточку двумя сообщениями, и вторым автору приходит обрывок комментария.
func TestCardFitsOneMessage(t *testing.T) {
	ticket := Ticket{Number: 68, Title: strings.Repeat("а", maxTitle),
		Author: "Иван", Status: Status{Title: "в работе"},
		Brief:   strings.Repeat("б", briefLimit),
		Comment: strings.Repeat("в", 5000),
		URL:     "https://example.invalid/issues/68"}

	text := cardText(&ticket)
	if runes := utf8.RuneCountInString(text); runes > maxMessage {
		t.Errorf("карточка не влезает в сообщение: %d рун", runes)
	}
	if !strings.Contains(text, "Полностью - в тикете") {
		t.Error("обрез комментария не помечен, автор примет обрывок за весь текст")
	}

	ticket.Comment = "Смотрю, к вечеру отвечу"
	if !strings.Contains(cardText(&ticket), ticket.Comment) {
		t.Error("короткий комментарий обрезан")
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
