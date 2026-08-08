package app

import (
	"embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Правила поведения - блок «Изменяемое»: состав пунктов правится без единой
// строки Go и без миграции. JSON, а не YAML: разбор есть в стандартной
// библиотеке, а пятая зависимость ради одного файла правил не окупается.
//
//go:embed rules/contract.json
var ruleFiles embed.FS

// caseKinds - типы обращения, которые принимает CHECK в cases.kind. Новый тип
// в правилах без миграции упал бы на первой же записи, поэтому список сверяется
// при загрузке.
var caseKinds = []string{"bug", "feature", "question"}

// ContractItem - пункт контракта готовности. Title работает дважды: вопрос
// интервью и заголовок раздела в теле issue. Одно место задаёт и что
// спрашиваем, и как это выглядит в тикете.
type ContractItem struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Required bool   `json:"required"`
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
	required := 0
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
		if it.Required {
			required++
		}
	}
	// Тип без единого обязательного пункта означает, что готовым считается любое
	// обращение: интервью тогда не задаёт ни одного вопроса.
	if required == 0 {
		return fmt.Errorf("contract kind %q has no required items", kind)
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

// Missing - обязательные пункты типа, которых нет в filled. Это второй,
// независимый от модели счёт пробелов: её собственный список gaps проверяется
// против него.
func (c Contract) Missing(kind string, filled map[string]string) []string {
	var gaps []string
	for _, it := range c[kind] {
		if !it.Required {
			continue
		}
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
			mark := "необязателен"
			if it.Required {
				mark = "обязателен"
			}
			fmt.Fprintf(&b, "- %s (%s): %s\n", it.Key, mark, it.Title)
		}
	}
	return strings.TrimSpace(b.String())
}
