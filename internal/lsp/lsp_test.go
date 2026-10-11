package lsp

import (
	"strings"
	"testing"
)

// Имена символов у языкового сервера приходят по-разному: `Close`,
// `(*Store).Close`, `Store.Close`, `Close(id int64)`. Человек пишет просто
// имя — сравнение должно это терпеть.
func TestBareName(t *testing.T) {
	cases := map[string]string{
		"Close":             "Close",
		"(*Store).Close":    "Close",
		"Store.Close":       "Close",
		"Close(id int64)":   "Close",
		"(e *Engine) Court": "Court",
		"*Engine":           "Engine",
		"attContext":        "attContext",
	}
	for in, want := range cases {
		if got := bareName(in); got != want {
			t.Errorf("bareName(%q) = %q, хотели %q", in, got, want)
		}
	}
}

func TestFindSymbolTolerant(t *testing.T) {
	syms := []Symbol{
		{Name: "Store", Kind: 23, Children: []Symbol{
			{Name: "(*Store).Close", Kind: 6},
			{Name: "Open(path string)", Kind: 12},
		}},
	}
	for _, want := range []string{"Close", "Open", "Store"} {
		if _, ok := FindSymbol(syms, want); !ok {
			t.Errorf("символ %q не нашёлся", want)
		}
	}
	if _, ok := FindSymbol(syms, "Нету"); ok {
		t.Error("нашёлся несуществующий символ")
	}
}

// Справочник языков: файл узнаётся по расширению, незнакомое — честный отказ.
func TestRegistryFor(t *testing.T) {
	r := DefaultRegistry()
	cases := map[string]string{
		"internal/store/store.go": "go",
		"src/main.rs":             "rust",
		"App.swift":               "swift",
		"components/Radar.vue":    "vue",
		"index.ts":                "typescript",
	}
	for path, want := range cases {
		l, ok := r.For(path)
		if !ok || l.Name != want {
			t.Errorf("For(%q) = %q, %v; хотели %q", path, l.Name, ok, want)
		}
	}
	if _, ok := r.For("readme.txt"); ok {
		t.Error("для .txt нашёлся язык")
	}
}

// Свой язык из настроек важнее умолчания: завтра вендор переименует
// бинарник, и это не повод пересобирать агента.
func TestRegistryOverride(t *testing.T) {
	r := DefaultRegistry()
	r.Add(Lang{Name: "go", Command: []string{"мой-gopls"}, Exts: []string{".go"}})
	l, ok := r.For("x.go")
	if !ok || l.Command[0] != "мой-gopls" {
		t.Fatalf("настройка не перебила умолчание: %+v", l)
	}
	r.Add(Lang{Name: "zig", Command: []string{"zls"}, Exts: []string{".zig"}})
	if l, ok := r.For("main.zig"); !ok || l.Name != "zig" {
		t.Error("новый язык не добавился")
	}
}

// Адреса документов: путь превращается в file://, а виртуальный адрес
// остаётся собой — файла на диске у него нет.
func TestURI(t *testing.T) {
	if got := PathToURI("/tmp/x.go"); got != "file:///tmp/x.go" {
		t.Errorf("PathToURI = %q", got)
	}
	virtual := "sema://module/rates.js"
	if got := PathToURI(virtual); got != virtual {
		t.Errorf("виртуальный адрес изменён: %q", got)
	}
	if got := URIToPath("file:///tmp/x.go"); got != "/tmp/x.go" {
		t.Errorf("URIToPath = %q", got)
	}
	if got := URIToPath(virtual); got != virtual {
		t.Errorf("виртуальный адрес изменён: %q", got)
	}
}

func TestWhere(t *testing.T) {
	w := Where("file:///tmp/x.go", Range{Start: Position{Line: 41}})
	if w != "/tmp/x.go:42" {
		t.Errorf("Where = %q, строки считаются с единицы", w)
	}
}

// Плоский список: вложенные символы не теряются и получают отступ.
func TestFlatten(t *testing.T) {
	syms := []Symbol{{Name: "Store", Kind: 23, Children: []Symbol{{Name: "Close", Kind: 6}}}}
	flat := Flatten(syms, 0)
	if len(flat) != 2 {
		t.Fatalf("символов %d", len(flat))
	}
	if flat[1].Depth != 1 || !strings.HasPrefix(flat[1].String(), "  ") {
		t.Errorf("вложенный символ без отступа: %q", flat[1].String())
	}
}
