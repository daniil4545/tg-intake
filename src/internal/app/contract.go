package app

import (
	"embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Правила поведения - блок «Изменяемое»: состав пунктов правится без единой
// строки Go и без миграции. JSON, а не YAML: разбор есть в стандартной
// библиотеке, а пятая зависимость ради одного файла правил не окупается.
//
//go:embed rules/*.json
var ruleFiles embed.FS

// caseKinds - типы обращения, которые принимает CHECK в cases.kind. Новый тип
// в правилах без миграции упал бы на первой же записи, поэтому список сверяется
// при загрузке.
var caseKinds = []string{"bug", "feature", "question", "mixed"}

// ContractItem - пункт ядра контракта готовности: всё, что в нём есть,
// обязательно. Остальное автор рассказывает сам, и оно живёт в свободных
// разделах тикета, а не в анкете. Title - название пункта в строке «Не
// уточнено» и в разделе, который Go дописывает за модель.
type ContractItem struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// Contract - пункты по типу обращения.
type Contract map[string][]ContractItem

// LoadContract читает правила при старте. Ошибка роняет сервис: пустой или
// битый контракт обнаружился бы на первом живом обращении, а не в логе выката.
func LoadContract() (Contract, error) {
	data, err := ruleFiles.ReadFile("rules/contract.json")
	if err != nil {
		return nil, fmt.Errorf("read contract rules: %w", err)
	}

	var c Contract
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("decode contract rules: %w", err)
	}
	if len(c) == 0 {
		return nil, fmt.Errorf("contract rules are empty")
	}

	for kind, items := range c {
		if !slices.Contains(caseKinds, kind) {
			return nil, fmt.Errorf("contract kind %q is not one of %s: new kind needs a migration of cases.kind",
				kind, strings.Join(caseKinds, ", "))
		}
		if err := checkItems(kind, items); err != nil {
			return nil, err
		}
	}
	for _, kind := range caseKinds {
		if len(c[kind]) == 0 {
			return nil, fmt.Errorf("contract kind %q has no items", kind)
		}
	}
	return c, nil
}

func checkItems(kind string, items []ContractItem) error {
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		switch {
		case it.Key == "":
			return fmt.Errorf("contract kind %q has an item without key", kind)
		case it.Title == "":
			return fmt.Errorf("contract item %s.%s has no title", kind, it.Key)
		case seen[it.Key]:
			return fmt.Errorf("contract kind %q has duplicate key %q", kind, it.Key)
		}
		seen[it.Key] = true
	}
	return nil
}

// Items отдаёт пункты типа; неизвестный тип даёт пустой список, и вызывающий
// обязан считать такой ответ модели невалидным.
func (c Contract) Items(kind string) []ContractItem { return c[kind] }

// Title - заголовок пункта. Пустой ответ означает ключ вне контракта.
func (c Contract) Title(kind, key string) string {
	for _, it := range c[kind] {
		if it.Key == key {
			return it.Title
		}
	}
	return ""
}

// Missing - пункты ядра типа, которых нет в filled. Это второй, независимый от
// модели счёт пробелов: её собственный список gaps проверяется против него.
func (c Contract) Missing(kind string, filled map[string]string) []string {
	var gaps []string
	for _, it := range c[kind] {
		if strings.TrimSpace(filled[it.Key]) == "" {
			gaps = append(gaps, it.Key)
		}
	}
	return gaps
}

// Prompt рендерит правила в кусок системного сообщения. Порядок типов
// фиксирован сортировкой: обход map даёт случайный порядок, а это изменяющаяся
// строка в кэшируемом префиксе.
func (c Contract) Prompt() string {
	kinds := make([]string, 0, len(c))
	for kind := range c {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)

	var b strings.Builder
	for _, kind := range kinds {
		fmt.Fprintf(&b, "\n%s:\n", kind)
		for _, it := range c[kind] {
			fmt.Fprintf(&b, "- %s: %s\n", it.Key, it.Title)
		}
	}
	return strings.TrimSpace(b.String())
}

// lowerFirst - название пункта внутри фразы: «Конкретный случай» в правилах,
// «..., конкретный случай» в строке.
func lowerFirst(text string) string {
	r, size := utf8.DecodeRuneInString(text)
	return string(unicode.ToLower(r)) + text[size:]
}
