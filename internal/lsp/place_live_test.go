package lsp

import (
	"context"
	"os"
	"testing"
	"time"
)

// Живая проверка на настоящем файле этой машины: включается переменной,
// потому что зависит от установленных языковых серверов.
func TestLiveHasSymbol(t *testing.T) {
	file := os.Getenv("SEMA_LIVE_FILE")
	if file == "" {
		t.Skip("нет SEMA_LIVE_FILE")
	}
	p := NewPool(DefaultRegistry(), 2, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, name := range []string{os.Getenv("SEMA_LIVE_SYM"), "ЭтогоСимволаТочноНет"} {
		found, known, tier := p.HasSymbol(ctx, os.Getenv("SEMA_LIVE_ROOT"), file, name)
		t.Logf("%q: есть=%v знаем=%v ярус=%s", name, found, known, tier)
	}
}
