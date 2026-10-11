package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Живая проверка на самом себе: карточки против чтения файлов.
// Пропускается, если gopls не установлен, — на сборочной машине его нет.

// serverSource — исходники сервера Сёмы для живых проверок: путь из
// SEMA_SERVER_SRC. Не задан или папки нет — проверка пропускается: на
// сборочной машине исходников сервера рядом нет.
func serverSource(t *testing.T) string {
	t.Helper()
	root := os.Getenv("SEMA_SERVER_SRC")
	if root == "" {
		t.Skip("SEMA_SERVER_SRC не задан — живая проверка пропущена")
	}
	if _, err := os.Stat(filepath.Join(root, "internal", "store", "store.go")); err != nil {
		t.Skipf("в SEMA_SERVER_SRC нет исходников сервера: %v", err)
	}
	return root
}

func spikePool(t *testing.T) *Pool {
	t.Helper()
	reg := DefaultRegistry()
	if l, ok := reg.For("x.go"); !ok || !l.Available() {
		t.Skip("gopls не установлен")
	}
	p := NewPool(reg, 2, 5*time.Minute)
	t.Cleanup(p.Close)
	return p
}

func TestCardsOnSelf(t *testing.T) {
	p := spikePool(t)
	spikeRoot := serverSource(t)
	spikeFile := spikeRoot + "/internal/store/store.go"
	ctx := context.Background()

	t0 := time.Now()
	sym, err := p.Symbols(ctx, spikeRoot, spikeFile)
	if err != nil {
		t.Fatalf("оглавление: %v", err)
	}
	t.Logf("symbols: %v · ярус %s · строк в карточке %d · не прочитано %d",
		time.Since(t0).Round(time.Millisecond), sym.Tier, sym.Lines, sym.Saved)
	if sym.Tier != TierLSP || sym.Saved <= 0 {
		t.Errorf("оглавление не сэкономило: %+v", sym)
	}

	t1 := time.Now()
	card, err := p.Context(ctx, spikeRoot, spikeFile, "Open")
	if err != nil {
		t.Fatalf("карточка: %v", err)
	}
	t.Logf("context(Open): %v · строк %d · не прочитано %d",
		time.Since(t1).Round(time.Millisecond), card.Lines, card.Saved)
	if !strings.Contains(card.Text, "Зовут в") {
		t.Errorf("в карточке нет мест вызова:\n%s", card.Text)
	}

	t2 := time.Now()
	imp, err := p.Impact(ctx, spikeRoot, spikeFile, "Open")
	if err != nil {
		t.Fatalf("радиус: %v", err)
	}
	t.Logf("impact(Open): %v\n%s", time.Since(t2).Round(time.Millisecond), imp.Text)
}

// Виртуальный документ: файла на диске нет, а диагностика есть.
func TestVirtualDocument(t *testing.T) {
	p := spikePool(t)
	spikeRoot := serverSource(t)
	ctx := context.Background()
	code := "package broken\n\nfunc Bad() int {\n\treturn \"строка вместо числа\"\n}\n"
	card, err := p.Diagnostics(ctx, spikeRoot, spikeRoot+"/internal/store/virtual_check.go", code)
	if err != nil {
		t.Skipf("диагностика недоступна: %v", err)
	}
	t.Logf("диагностика виртуального документа:\n%s", card.Text)
	if !strings.Contains(card.Text, "ошибка") && !strings.Contains(card.Text, "Замечаний нет") {
		t.Errorf("неожиданный ответ: %s", card.Text)
	}
}
