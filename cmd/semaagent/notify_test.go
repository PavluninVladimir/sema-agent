package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/PavluninVladimir/sema-agent/internal/desk"
)

// Номер последней показанной записи переживает перезапуск: иначе после
// подъёма житель машины показал бы человеку вчерашнее заново.
func TestShownNotShownAgain(t *testing.T) {
	home := t.TempDir()
	n := newNotifier(home, nil)
	n.save(12)
	n.save(7) // назад не двигаемся: 7 показали раньше 12

	raw, err := os.ReadFile(filepath.Join(home, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st deskState
	if json.Unmarshal(raw, &st) != nil || st.Last != 12 {
		t.Fatalf("состояние не то: %s", raw)
	}
	again := newNotifier(home, nil)
	if again.last != 12 {
		t.Fatalf("после перезапуска забыли показанное: %d", again.last)
	}
}

// Показать не удалось — отвечаем «не показал» с причиной, а не молчим:
// Сёма по этому ответу решает, помечать ли запись доставленной.
func TestFailureNamesReason(t *testing.T) {
	t.Setenv("SEMA_DESK_QUIET", "1") // показывать некому и незачем: проверяем ответ
	n := &notifier{desk: desk.New(t.TempDir(), nil, nil), home: t.TempDir()}
	res, err := n.show(desk.Card{ID: 5, Title: "проба"})
	if err != nil {
		t.Fatalf("вызов упал вместо честного ответа: %v", err)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("ответ не тот: %+v", res)
	}
	if shown, _ := m["показал"].(bool); shown {
		t.Fatal("показал, хотя показывать нечем")
	}
	if why, _ := m["почему"].(string); why == "" {
		t.Fatal("отказ без причины")
	}
}
