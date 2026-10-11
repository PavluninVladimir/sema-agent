package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Счёт по дням: по нарастающему итогу не видно, стали ли карточки
// спрашивать после подсказки в хуке. Неделя считается по дневным
// строкам, карточки с приложением — отдельно, файл переживает перезапуск.
func TestSavingsByDays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "savings.json")
	s := &server{}
	s.loadSavings(path)
	s.note(10, 100, false)
	s.note(20, 200, true)
	got := s.savedNow()
	if got.Calls != 2 || got.Lines != 300 || got.WithKnowledge != 1 {
		t.Fatalf("итог: %+v", got)
	}
	today := time.Now().Format("2006-01-02")
	if d := got.Days[today]; d.Calls != 2 || d.Lines != 300 || d.WithKnowledge != 1 {
		t.Fatalf("строка дня: %+v", d)
	}
	// старая строка вычищается, свежая входит в неделю
	s.mu.Lock()
	s.saved.Days["2000-01-01"] = daySavings{Calls: 99}
	s.mu.Unlock()
	s.note(1, 1, false)
	got = s.savedNow()
	if _, ok := got.Days["2000-01-01"]; ok {
		t.Fatalf("строка тридцатидневной давности осталась")
	}
	if w := got.savedWeek(); w.Calls != 3 || w.WithKnowledge != 1 {
		t.Fatalf("неделя: %+v", w)
	}
	// перезапуск поднимает счёт с диска вместе с днями
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back savings
	if err := json.Unmarshal(raw, &back); err != nil || back.Days[today].Calls != 3 {
		t.Fatalf("на диске не то: %s (%v)", raw, err)
	}
}
