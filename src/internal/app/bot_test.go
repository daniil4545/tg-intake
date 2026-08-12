package app

import "testing"

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
