package app

import (
	"strings"
	"testing"
)

// TestOverlapKeepsOnlyRead: номер тикета и путь файла называет модель, а не
// человек. Пункт про то, чего сервис не читал, - выдумка: автор примет её за
// факт и не заведёт нужный тикет.
func TestOverlapKeepsOnlyRead(t *testing.T) {
	issues := []Issue{{Number: 57}, {Number: 68}}
	loaded := []docText{{Path: "CHANGELOG.md"}}

	kept, dropped := keepFound([]overlapItem{
		{Issue: 57, Note: "та же механика"},
		{Issue: 999, Note: "выдуманный номер"},
		{Path: "CHANGELOG.md", Note: "вышло в 0.4.2"},
		{Path: "docs/выдумка.md", Note: "выдуманный путь"},
		{Issue: 68, Note: ""},
	}, issues, loaded)

	if len(kept) != 2 {
		t.Fatalf("прошло пунктов: %d, ожидалось 2: %+v", len(kept), kept)
	}
	if kept[0].Issue != 57 || kept[1].Path != "CHANGELOG.md" {
		t.Errorf("прошли не те пункты: %+v", kept)
	}
	if dropped != 3 {
		t.Errorf("отброшено пунктов: %d, ожидалось 3", dropped)
	}
}

// TestOverlapListBuildsLinks: адреса собирает Go по номеру и пути - модель их не
// пишет вовсе. Закрытый тикет помечается: для автора это разница между «уже
// делают» и «сделано или отклонено».
func TestOverlapListBuildsLinks(t *testing.T) {
	project := Project{Owner: "acme", Repo: "proj"}
	issues := []Issue{{
		Number: 57, State: "closed", Title: "Автологирование причин отказов",
		HTMLURL: "https://github.com/acme/proj/issues/57",
	}}

	list := overlapList([]overlapItem{
		{Issue: 57, Note: "та же механика"},
		{Path: "CHANGELOG.md", Note: "вышло в 0.4.2"},
	}, issues, project, "main")

	if !strings.Contains(list, "https://github.com/acme/proj/issues/57") {
		t.Errorf("нет ссылки на тикет:\n%s", list)
	}
	if !strings.Contains(list, "Автологирование причин отказов") || !strings.Contains(list, "закрыт") {
		t.Errorf("тикет без заголовка или без пометки о закрытии:\n%s", list)
	}
	if !strings.Contains(list, "https://github.com/acme/proj/blob/main/CHANGELOG.md") {
		t.Errorf("нет ссылки на документ:\n%s", list)
	}
}

// TestOverlapEmptyWithoutItems: сверка ничего не нашла - карточка саммари обязана
// выглядеть как до среза. Пустой список пунктов не имеет права стать блоком с
// заголовком и пустотой под ним.
func TestOverlapEmptyWithoutItems(t *testing.T) {
	kept, dropped := keepFound([]overlapItem{{Issue: 999, Note: "выдумка"}},
		[]Issue{{Number: 57}}, nil)

	if len(kept) != 0 || dropped != 1 {
		t.Fatalf("пунктов: %d, отброшено: %d", len(kept), dropped)
	}
	if list := overlapList(kept, nil, Project{}, "main"); list != "" {
		t.Errorf("пустой список пересечений дал текст: %q", list)
	}
	if strings.Contains(summaryMessage("Заголовок", "", "## Случай\n\nтекст", nil, false, ""),
		"уже есть") {
		t.Error("карточка саммари обещает пересечения, которых нет")
	}
}

// TestIssueBodyKeepsOverlap: список пересечений уходит в тело тикета отдельным
// разделом - ради него срез и делается: берущий тикет видит найденный контекст,
// не восстанавливая его с нуля. Место раздела фиксировано: после ссылок автора и
// до пробелов контракта.
func TestIssueBodyKeepsOverlap(t *testing.T) {
	publisher := NewPublisher(nil, nil, testRules(t), testLog(t), 0)
	cs := &Case{
		Kind: "feature", Summary: "## Случай\n\nНужно логировать отказы",
		Overlap: "- [Тикет #57 Автологирование](https://github.com/acme/proj/issues/57), закрыт: та же механика",
		Gaps:    []string{"steps"},
	}

	body := publisher.body(cs, User{First: "Иван"}, []string{"https://crm/lead/1"}, "<!-- marker -->")

	overlap := strings.Index(body, "## Пересечения")
	if overlap < 0 || !strings.Contains(body, "Тикет #57") {
		t.Fatalf("пересечений нет в теле тикета:\n%s", body)
	}
	if links := strings.Index(body, "## Ссылки"); links > overlap {
		t.Errorf("пересечения идут раньше ссылок автора:\n%s", body)
	}
	if gaps := strings.Index(body, "## Не разобрано"); gaps < overlap {
		t.Errorf("пересечения идут после пробелов контракта:\n%s", body)
	}

	cs.Overlap = ""
	if strings.Contains(publisher.body(cs, User{First: "Иван"}, nil, "<!-- marker -->"), "## Пересечения") {
		t.Error("пустая сверка оставила в тикете пустую рубрику")
	}
}

// TestSummaryShowsOverlapPlain: список хранится в markdown ради тела issue, а в
// чат уходит без разметки - parse_mode сервис не включает, и ссылка обязана
// остаться читаемой сама по себе.
func TestSummaryShowsOverlapPlain(t *testing.T) {
	overlap := "- [Тикет #57 Автологирование](https://github.com/acme/proj/issues/57): та же механика"

	card := summaryMessage("Заголовок", "", "## Случай\n\nтекст", nil, false, overlap)

	if !strings.Contains(card, "Тикет #57 Автологирование (https://github.com/acme/proj/issues/57)") {
		t.Errorf("ссылка в карточке осталась разметкой:\n%s", card)
	}
	if strings.Index(card, "уже есть") > strings.Index(card, "Где я ошибся") {
		t.Errorf("пересечения показаны после вопроса о правке:\n%s", card)
	}
}
