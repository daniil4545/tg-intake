package app

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// TestMarkWaited: предупреждение о задержке одно на ход, но следующий ход и
// следующее обращение получают своё. Раунд нового обращения снова нулевой, и
// счёт по одному номеру оставил бы автора без предупреждения со второго
// обращения и дальше.
func TestMarkWaited(t *testing.T) {
	b := &Bot{waited: map[int64]string{}}
	const user = int64(7)

	if !b.markWaited(user, "case-1", 0) {
		t.Fatal("первое ожидание обязано предупредить")
	}
	if b.markWaited(user, "case-1", 0) {
		t.Error("второй ответ в том же ходе предупредил повторно")
	}
	if !b.markWaited(user, "case-1", 1) {
		t.Error("следующий раунд остался без предупреждения")
	}
	if !b.markWaited(user, "case-2", 0) {
		t.Error("новое обращение осталось без предупреждения")
	}
}

// TestNotifyAlertThread: тема ставится, только если работа адресована
// действующему чату уведомлений (§2 плана alert-topic) - work, поставленная в
// прежний чат до выката, чужую тему не получает.
func TestNotifyAlertThread(t *testing.T) {
	ctx := context.Background()
	const otherChat = testAlertChat - 1

	cases := []struct {
		name       string
		chatID     int64
		thread     int
		wantThread string // "" - message_thread_id в запросе нет
	}{
		{"with_thread", testAlertChat, 5, "5"},
		{"no_thread", testAlertChat, 0, ""},
		{"other_chat", otherChat, 5, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ft, tb := newFakeTelegram(t)
			log, _ := screenLog()
			b := &Bot{alert: tb, alertChat: testAlertChat, alertThread: c.thread, log: log}

			raw, err := json.Marshal(notifyPayload{CaseID: "case-1", Text: "алерт", ChatID: c.chatID})
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			if err := b.Notify(ctx, Job{ID: 1, Kind: JobNotify, Payload: raw}); err != nil {
				t.Fatalf("notify: %v", err)
			}

			sends := ft.methodCalls("sendMessage")
			if len(sends) != 1 {
				t.Fatalf("sendMessage calls: %d, want 1", len(sends))
			}
			_, hasThread := sends[0].body["message_thread_id"]
			if c.wantThread == "" {
				if hasThread {
					t.Errorf("message_thread_id не ожидался: %v", sends[0].body)
				}
				return
			}
			if got := fmt.Sprint(sends[0].body["message_thread_id"]); got != c.wantThread {
				t.Errorf("message_thread_id: got %q, want %q", got, c.wantThread)
			}
		})
	}
}

// TestNotifyAuthorIgnoresAlertThread: сообщение автору (ChatID == 0) не
// адресовано отправителю алертов и темы не получает, даже когда она
// настроена - рубеж 3a плана alert-topic.
func TestNotifyAuthorIgnoresAlertThread(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	cases := newTestCases(t, pool, t.TempDir())
	ft, tb := newFakeTelegram(t)
	log, _ := screenLog()
	b := screenBot(tb, pool, cases, log)
	b.alertThread = 5

	cs := startInterview(t, cases, 8500, 2)

	if err := b.Notify(ctx, notifyRoundJob(t, cs.ID, cs.Round, "Раунд вопросов", keysRound)); err != nil {
		t.Fatalf("notify: %v", err)
	}

	sends := ft.methodCalls("sendMessage")
	if len(sends) == 0 {
		t.Fatal("сообщение автору не отправлено")
	}
	if _, has := sends[len(sends)-1].body["message_thread_id"]; has {
		t.Errorf("сообщению автору досталась тема алертов: %v", sends[len(sends)-1].body)
	}
}
