package lsp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Символ ищется по оглавлению файла, и «не знаю» отличается от «нет».
// Без языкового сервера и без ctags ответ обязан быть «не знаю»: по
// молчанию Сёма координаты памяти не снимает, иначе одна ненастроенная
// машина стёрла бы всю разметку.
func TestHasSymbolUnknownWithoutTools(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "проба.неизвестный")
	if err := os.WriteFile(file, []byte("func Значок() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewPool(&Registry{}, 1, time.Minute)
	p.SetTools(Tools{})
	found, known, _ := p.HasSymbol(context.Background(), dir, file, "Значок")
	if known || found {
		t.Fatalf("без инструментов ответ должен быть «не знаю», получили есть=%v знаем=%v", found, known)
	}
	// пустое имя символа спрашивать не о чем
	if _, known, _ := p.HasSymbol(context.Background(), dir, file, ""); known {
		t.Error("про пустой символ агент что-то знает")
	}
}
