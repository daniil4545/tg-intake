package app

import "testing"

// TestParseStatuses: битые правила роняют старт. Обнаружить их на первом
// просмотре означало бы показать автору «статус недоступен» вместо статуса.
func TestParseStatuses(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"валидный набор", `{"statuses":[
			{"label":"status:new","title":"Заведён"},
			{"label":"status:cancelled","title":"Отменён","final":true}]}`, true},
		{"пустой список", `{"statuses":[]}`, false},
		{"метка без префикса", `{"statuses":[
			{"label":"new","title":"Заведён"},
			{"label":"status:cancelled","title":"Отменён","final":true}]}`, false},
		{"дубль метки", `{"statuses":[
			{"label":"status:new","title":"Заведён"},
			{"label":"status:new","title":"Ещё раз"},
			{"label":"status:cancelled","title":"Отменён","final":true}]}`, false},
		{"пустой заголовок", `{"statuses":[
			{"label":"status:new","title":""},
			{"label":"status:cancelled","title":"Отменён","final":true}]}`, false},
		{"только финальные", `{"statuses":[
			{"label":"status:cancelled","title":"Отменён","final":true}]}`, false},
		{"без метки отмены", `{"statuses":[{"label":"status:new","title":"Заведён"}]}`, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseStatuses([]byte(c.raw))
			if c.ok && err != nil {
				t.Errorf("want no error, got: %v", err)
			}
			if !c.ok && err == nil {
				t.Error("want error, got nil")
			}
		})
	}
}

// TestPickPriority: порядок меток в ответе GitHub произволен, приоритет задаёт
// файл правил. Две метки статуса на одном issue - обычное дело после падения
// между добавлением новой и снятием старой.
func TestPickPriority(t *testing.T) {
	statuses := Statuses{
		{Label: "status:cancelled", Title: "Отменён", Final: true},
		{Label: "status:in-progress", Title: "В работе"},
		{Label: "status:new", Title: "Заведён"},
	}

	got, ok := statuses.Pick([]string{"type:bug", "status:in-progress", "status:cancelled"})
	if !ok || got.Label != "status:cancelled" {
		t.Errorf("побеждает первый по файлу: получили %q, ok=%v", got.Label, ok)
	}

	if _, ok := statuses.Pick([]string{"type:bug", "author:ivan", "incomplete"}); ok {
		t.Error("метки без префикса статусом быть не могут")
	}
}

// TestUnknownLabels: в лог попадает только метка статуса вне правил. Иначе
// type и author, которые висят на каждом тикете, залили бы его мусором.
func TestUnknownLabels(t *testing.T) {
	statuses := Statuses{{Label: "status:new", Title: "Заведён"}}

	got := statuses.Unknown([]string{"type:bug", "author:ivan", "incomplete", "status:frozen"})
	if len(got) != 1 || got[0] != "status:frozen" {
		t.Errorf("неизвестные метки: %v, ожидалась только status:frozen", got)
	}
}
