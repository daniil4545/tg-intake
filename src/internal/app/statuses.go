package app

import (
	"encoding/json"
	"fmt"
	"strings"
)

// statusPrefix - признак метки статуса. Без него в «неизвестный статус» попадали
// бы type: и author:, которые висят на каждом тикете.
const statusPrefix = "status:"

// Status - состояние тикета. Единственный источник истины по нему - метка
// issue; здесь только то, как её показать автору и как завести в репозитории.
type Status struct {
	Label string `json:"label"`
	Title string `json:"title"`
	Color string `json:"color"`
	Final bool   `json:"final"`
}

// Statuses - статусы в порядке приоритета: побеждает первый по файлу. Порядок
// меток в ответе GitHub произволен и приоритетом быть не может.
type Statuses []Status

// LoadStatuses читает правила при старте. Ошибка роняет сервис: битый набор
// статусов обнаружился бы на первом просмотре, а не в логе выката.
func LoadStatuses() (Statuses, error) {
	data, err := ruleFiles.ReadFile("rules/statuses.json")
	if err != nil {
		return nil, fmt.Errorf("read status rules: %w", err)
	}
	return parseStatuses(data)
}

func parseStatuses(data []byte) (Statuses, error) {
	var file struct {
		Statuses Statuses `json:"statuses"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("decode status rules: %w", err)
	}
	if len(file.Statuses) == 0 {
		return nil, fmt.Errorf("status rules are empty")
	}

	seen := make(map[string]bool, len(file.Statuses))
	open := 0
	for _, s := range file.Statuses {
		switch {
		case s.Label == "":
			return nil, fmt.Errorf("status rules have an entry without label")
		case !strings.HasPrefix(s.Label, statusPrefix):
			return nil, fmt.Errorf("status label %q must start with %q", s.Label, statusPrefix)
		case s.Title == "":
			return nil, fmt.Errorf("status %q has no title", s.Label)
		case seen[s.Label]:
			return nil, fmt.Errorf("status rules have duplicate label %q", s.Label)
		}
		seen[s.Label] = true
		if !s.Final {
			open++
		}
	}
	// Набор из одних финальных статусов молча убрал бы кнопку отмены навсегда.
	if open == 0 {
		return nil, fmt.Errorf("status rules have no non-final status")
	}
	// Метку отмены ставит код, и её отсутствие в правилах означало бы статус,
	// который бот проставит, но не сумеет показать.
	if !seen[labelCancelled] {
		return nil, fmt.Errorf("status rules have no %q", labelCancelled)
	}
	return file.Statuses, nil
}

// Pick - статус тикета по меткам issue. Метки без префикса пропускаются молча:
// type и author есть на каждом тикете и статусом не являются.
func (s Statuses) Pick(labels []string) (Status, bool) {
	for _, status := range s {
		for _, label := range labels {
			if label == status.Label {
				return status, true
			}
		}
	}
	return Status{}, false
}

// Unknown - метки статуса, которых нет в правилах. Владелец завёл свою метку, и
// выдумывать её смысл сервис не станет, но в логе это должно быть видно.
func (s Statuses) Unknown(labels []string) []string {
	var unknown []string
	for _, label := range labels {
		if !strings.HasPrefix(label, statusPrefix) {
			continue
		}
		if _, ok := s.Pick([]string{label}); !ok {
			unknown = append(unknown, label)
		}
	}
	return unknown
}
