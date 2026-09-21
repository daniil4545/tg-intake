package app

import (
	"encoding/json"
	"maps"
	"math"
	"strings"
	"testing"
)

// caseRun - итог одного обращения в прогоне eval.
type caseRun struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`       // тип после саммари
	Questions  []int  `json:"questions"`  // число вопросов по раундам, пусто - без вопросов
	Incomplete bool   `json:"incomplete"` // cases.incomplete после Summarize
	Failed     string `json:"failed,omitempty"`
}

// evalEvent - событие журнала исходника в наборе eval.
type evalEvent struct {
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// roundAnswers - ответы автора исходника по номеру раунда до первого саммари:
// правки саммари отвечают на другое и в ответ раунда не попадают (Р-12).
// Раунд 0 - написанное до первого вопроса.
func roundAnswers(events []evalEvent) map[int]string {
	answers := map[int]string{}
	for _, e := range events {
		if e.Kind == "interview_done" || e.Kind == "summary_ready" {
			break
		}
		if e.Kind != "answer_given" {
			continue
		}
		var p struct {
			Round int    `json:"round"`
			Text  string `json:"text"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || strings.TrimSpace(p.Text) == "" {
			continue
		}
		if answers[p.Round] != "" {
			answers[p.Round] += "\n"
		}
		answers[p.Round] += p.Text
	}
	return answers
}

// evalMetrics - M1-M4 по спеке ticket-form, Р-9. Знаменатели лежат рядом:
// доля от пяти обращений и доля от пятидесяти сравниваются по-разному.
type evalMetrics struct {
	M1     float64 `json:"m1"` // доля обращений без вопросов
	M2     float64 `json:"m2"` // среднее число раундов до саммари
	M3     float64 `json:"m3"` // доля bug и mixed с закрытым ядром
	M4     float64 `json:"m4"` // среднее число вопросов в раунде 1
	Cases  int     `json:"cases"`
	Bugs   int     `json:"bugs"`   // знаменатель M3
	Asked  int     `json:"asked"`  // знаменатель M4
	Failed int     `json:"failed"` // в метрики не входят
}

func measure(runs []caseRun) evalMetrics {
	var m evalMetrics
	var silent, rounds, complete, firstRound int
	for _, r := range runs {
		if r.Failed != "" {
			m.Failed++
			continue
		}
		m.Cases++
		rounds += len(r.Questions)
		if len(r.Questions) == 0 {
			silent++
		} else {
			m.Asked++
			firstRound += r.Questions[0]
		}
		if r.Kind == "bug" || r.Kind == "mixed" {
			m.Bugs++
			if !r.Incomplete {
				complete++
			}
		}
	}
	m.M1 = share(silent, m.Cases)
	m.M2 = share(rounds, m.Cases)
	m.M3 = share(complete, m.Bugs)
	m.M4 = share(firstRound, m.Asked)
	return m
}

// share - частное с нулём на пустом знаменателе: JSON не умеет NaN, а пустой
// знаменатель виден рядом в самих метриках.
func share(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

// TestEvalMetrics: обращения без failed - без вопросов, два раунда, bug с
// неполным ядром; failed в знаменатели не входит.
func TestEvalMetrics(t *testing.T) {
	got := measure([]caseRun{
		{ID: "silent", Kind: "feature"},
		{ID: "two-rounds", Kind: "bug", Questions: []int{3, 1}},
		{ID: "incomplete", Kind: "bug", Questions: []int{2}, Incomplete: true},
		{ID: "broken", Kind: "bug", Questions: []int{3, 3}, Failed: "no progress"},
	})
	want := evalMetrics{
		M1: 1.0 / 3, M2: 1, M3: 0.5, M4: 2.5,
		Cases: 3, Bugs: 2, Asked: 2, Failed: 1,
	}
	const eps = 1e-9
	if math.Abs(got.M1-want.M1) > eps || math.Abs(got.M2-want.M2) > eps ||
		math.Abs(got.M3-want.M3) > eps || math.Abs(got.M4-want.M4) > eps ||
		got.Cases != want.Cases || got.Bugs != want.Bugs || got.Asked != want.Asked || got.Failed != want.Failed {
		t.Fatalf("measure:\n got %+v\nwant %+v", got, want)
	}
}

// TestRoundAnswers: ответ раунда по журналу исходника (Р-12).
func TestRoundAnswers(t *testing.T) {
	answer := func(round int, text string) evalEvent {
		payload, _ := json.Marshal(map[string]any{"round": round, "text": text})
		return evalEvent{Kind: "answer_given", Payload: payload}
	}
	asked := evalEvent{Kind: "round_asked", Payload: json.RawMessage(`{"round":1}`)}
	tests := []struct {
		name   string
		events []evalEvent
		want   map[int]string
	}{
		{"fix after summary is dropped",
			[]evalEvent{asked, answer(1, "a"), {Kind: "summary_ready"}, answer(1, "fix")},
			map[int]string{1: "a"}},
		{"fix after interview done is dropped",
			[]evalEvent{asked, answer(1, "a"), {Kind: "interview_done"}, answer(1, "fix")},
			map[int]string{1: "a"}},
		{"two answers of a round are joined",
			[]evalEvent{asked, answer(1, "a"), answer(1, "b")},
			map[int]string{1: "a\nb"}},
		{"empty text is skipped",
			[]evalEvent{asked, answer(1, " "), answer(1, "a")},
			map[int]string{1: "a"}},
		{"round zero is collected",
			[]evalEvent{answer(0, "before"), asked, answer(1, "a")},
			map[int]string{0: "before", 1: "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := roundAnswers(tt.events); !maps.Equal(got, tt.want) {
				t.Fatalf("roundAnswers = %q, want %q", got, tt.want)
			}
		})
	}
}
