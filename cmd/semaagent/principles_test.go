package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Абзац с двумя путями даёт две привязки одного заголовка; переписанный
// абзац — другой отпечаток; заголовка без путей не бывает в выдаче.
func TestParsePrinciples(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string, dir bool) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if dir {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			return
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("server/.git", true)
	mk("server/internal/store/queue.go", false)
	mk("server/internal/engine/queue.go", false)
	mk("server/internal/web/x.go", false)
	mk("server/internal/api/ui.go", false)
	mk("server/internal/api/inbox.go", false)
	text := "# CLAUDE.md\n\n## Архитектура\n\n" +
		"**Очередь фоновой работы** (`store/queue.go`, `engine/queue.go`,\n`api/queue.go`). Всё, что делается не сразу, стоит строкой в\n`work_queue`: вид, объект, приоритет. Пара «вид + объект» уникальна.\n\n" +
		"Просто абзац без заголовка про `store/queue.go` — не принцип.\n\n" +
		"**Server-driven UI.** `server/internal/api/ui.go` описывает DSL. Экраны собираются по файлам: `inbox.go`, `files.go`. Веб-клиент (`internal/web`) рендерит компоненты, не зная предметной области.\n\n" +
		"- **Никакого cgo.** SQLite — modernc.org/sqlite.\n"
	items := parsePrinciples(text, umbrellaResolver(root))
	byHead := map[string][]principle{}
	for _, it := range items {
		byHead[it.Heading] = append(byHead[it.Heading], it)
	}
	q := byHead["Очередь фоновой работы"]
	if len(q) != 2 {
		t.Fatalf("у абзаца с двумя существующими путями ждали две привязки (api/queue.go нет на диске): %+v", items)
	}
	if q[0].File != "internal/store/queue.go" || q[0].Repo != "server" || q[1].File != "internal/engine/queue.go" {
		t.Fatalf("пути не разрешены от корня репозитория: %+v", q)
	}
	if q[0].Hash == "" || q[0].Hash != q[1].Hash {
		t.Fatalf("у одного абзаца разные отпечатки: %+v", q)
	}
	if q[0].Text != "Всё, что делается не сразу, стоит строкой в work_queue: вид, объект, приоритет." {
		t.Fatalf("строка итога — первое предложение после заголовка и скобок с путями: %q", q[0].Text)
	}
	ui := byHead["Server-driven UI"]
	// ui.go с папкой, inbox.go голым именем — в папке, которую назвал абзац;
	// files.go на диске нет — не выдумывается; internal/web — папка
	if len(ui) != 3 {
		t.Fatalf("ждали ui.go, inbox.go и папку internal/web: %+v", ui)
	}
	if ui[0].File != "internal/api/ui.go" || ui[1].Dir != "internal/web" || ui[1].File != "" {
		t.Fatalf("пути или папка не те: %+v", ui)
	}
	if ui[2].File != "internal/api/inbox.go" || ui[2].Repo != "server" {
		t.Fatalf("голое имя файла не нашлось в папке абзаца: %+v", ui)
	}
	if ui[0].Text != "server/internal/api/ui.go описывает DSL." {
		t.Fatalf("строка итога: %q", ui[0].Text)
	}
	cgo := parsePrinciples("- **Никакого cgo.** SQLite — `internal/store/queue.go`.\n", umbrellaResolver(root))
	if len(cgo) == 0 || strings.HasPrefix(cgo[0].Text, "-") {
		t.Fatalf("маркер списка попал в строку итога: %+v", cgo)
	}
	if _, ok := byHead["Никакого cgo"]; ok {
		t.Fatal("абзац без путей дал привязку")
	}
	if len(byHead) != 2 {
		t.Fatalf("лишние заголовки: %v", byHead)
	}
	// путь с именем репозитория впереди — внутрь репозитория, как его
	// называет карточка: репозиторий server, файл от его корня
	full := parsePrinciples("**С приставкой** — `server/internal/web/x.go` и папка `server/`.\n", umbrellaResolver(root))
	if len(full) != 2 || full[0].Repo != "server" || full[0].File != "internal/web/x.go" {
		t.Fatalf("путь с именем репозитория не разрешён внутрь него: %+v", full)
	}
	if full[1].Repo != root[strings.LastIndex(root, "/")+1:] || full[1].Dir != "server" {
		t.Fatalf("папка самого репозитория — место зонтика: %+v", full)
	}
	// переписали абзац — отпечаток другой, перенос строки в другом месте — тот же
	a := parsePrinciples("**Очередь фоновой работы** (`store/queue.go`). Раз два\nтри.\n", umbrellaResolver(root))
	b := parsePrinciples("**Очередь фоновой работы** (`store/queue.go`). Раз\nдва три.\n", umbrellaResolver(root))
	c := parsePrinciples("**Очередь фоновой работы** (`store/queue.go`). Совсем другое.\n", umbrellaResolver(root))
	if a[0].Hash != b[0].Hash {
		t.Fatal("перенос строки изменил отпечаток")
	}
	if a[0].Hash == c[0].Hash {
		t.Fatal("переписанный абзац сохранил отпечаток")
	}
}
