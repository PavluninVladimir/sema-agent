package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Стенограмма Claude Code: последние две реплики уходят в сводку. Куски
// с вызовами инструментов текстом не считаются — иначе в память поедет
// JSON вместо разговора.
func TestTranscriptTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "стенограмма.jsonl")
	body := strings.Join([]string{
		`{"type":"user","message":{"content":"первый вопрос"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"первый ответ"}]}}`,
		`{"type":"system","message":{"content":"шум"}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","text":"вывод"}]}}`,
		`{"type":"user","message":{"content":"второй вопрос"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"второй ответ"}]}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := noteText("note", "Claude Code", hookEvent{TranscriptPath: path})
	want := "User: второй вопрос\nAgent: второй ответ"
	if got != want {
		t.Fatalf("хвост разобран не так:\n%q\nждали\n%q", got, want)
	}

	// итог субагента — его последняя реплика: работа целого агента иначе
	// не попадает в память ни строкой
	sub := noteText("subagent-note", "Claude Code", hookEvent{TranscriptPath: path, AgentType: "ревью"})
	if sub != "Субагент (ревью): второй ответ" {
		t.Fatalf("итог субагента: %q", sub)
	}
}

// Codex отдаёт реплики прямо в хук: стенограмму разбирать не нужно.
func TestCodexNote(t *testing.T) {
	if got := noteText("note", "Codex", hookEvent{Prompt: "почини сборку"}); got != "User: почини сборку" {
		t.Fatalf("реплика человека: %q", got)
	}
	if got := noteText("note", "Codex", hookEvent{LastAssistant: "починил"}); got != "Agent: починил" {
		t.Fatalf("реплика агента: %q", got)
	}
}

// Обрезка по рунам, а не по байтам: русский текст иначе режется вдвое
// раньше и доезжает половиной слова.
func TestClipByRunes(t *testing.T) {
	if got := clip("абвгд", 3); got != "абв" {
		t.Fatalf("обрезали по байтам: %q", got)
	}
}

// Житель машины не отвечает — хук молчит и не роняет сессию агента
// разработки: ни паники, ни ненулевого кода.
func TestHookWithoutDaemon(t *testing.T) {
	home := t.TempDir()
	got := runHook("session-start", "Claude Code", home, strings.NewReader(`{"cwd":"`+home+`"}`))
	if got != "" {
		t.Fatalf("без демона хук что-то придумал: %q", got)
	}
	// а Codex всё равно получает разбираемый JSON, иначе он ругается
	printHook("session-start", "Codex", got)
}

// Подсказка о карточках: Claude Code получает и строку про загрузку
// отложенных инструментов, Codex — нет (у него инструменты видны сразу).
// Памяти нет — подсказка всё равно есть: карточки живут без Сёмы.
func TestCardsHint(t *testing.T) {
	mem := "Память Сёмы: урок такой-то"
	claude := sessionStartText("Claude Code", mem)
	if !strings.HasPrefix(claude, mem) {
		t.Fatalf("память должна идти первой: %q", claude)
	}
	if !strings.Contains(claude, "symbols") || !strings.Contains(claude, "ToolSearch") {
		t.Fatalf("Claude Code без подсказки или без строки про загрузку: %q", claude)
	}
	codex := sessionStartText("Codex", mem)
	if !strings.Contains(codex, "symbols") || strings.Contains(codex, "ToolSearch") {
		t.Fatalf("Codex: подсказка нужна, ToolSearch — нет: %q", codex)
	}
	without := sessionStartText("Claude Code", "")
	if !strings.Contains(without, "symbols") || strings.HasPrefix(without, "\n") {
		t.Fatalf("без памяти подсказка должна быть и без пустого начала: %q", without)
	}
	if n := strings.Count(strings.TrimPrefix(claude, mem), "\n"); n > 6 {
		t.Fatalf("подсказка длиннее пяти строк: %d переводов", n)
	}
}

// Одна огромная реплика не должна уносить весь хвост стенограммы:
// у Scanner на этом месте был предел на длину строки, и разбор молча
// останавливался.
func TestHugeMessageDoesNotBreakParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "стенограмма.jsonl")
	huge, _ := json.Marshal(map[string]any{"type": "assistant",
		"message": map[string]any{"content": strings.Repeat("щ", 9<<20)}})
	body := strings.Join([]string{
		string(huge),
		`{"type":"user","message":{"content":"вопрос после огромной"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"ответ после огромной"}]}}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := noteText("note", "Claude Code", hookEvent{TranscriptPath: path})
	if got != "User: вопрос после огромной\nAgent: ответ после огромной" {
		t.Fatalf("хвост потерян: %q", got)
	}
}

// Итог субагента бывает в своей стенограмме: в общей его работы нет.
func TestSubagentSummaryFromOwnTranscript(t *testing.T) {
	dir := t.TempDir()
	own := filepath.Join(dir, "субагент.jsonl")
	if err := os.WriteFile(own, []byte(`{"type":"assistant","message":{"content":"нашёл три дыры"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	common := filepath.Join(dir, "общая.jsonl")
	if err := os.WriteFile(common, []byte(`{"type":"assistant","message":{"content":"жду субагента"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := noteText("subagent-note", "Claude Code",
		hookEvent{TranscriptPath: common, AgentTranscript: own, AgentType: "ревьюер"})
	if got != "Субагент (ревьюер): нашёл три дыры" {
		t.Fatalf("взяли не ту стенограмму: %q", got)
	}
}

// Файл координаты ищется во всех рабочих папках и с приставкой
// репозитория: у старых записей путь от корня репозитория, у новых — от
// корня зонтика. Не нашли — так и говорим, гадать по всей машине нельзя.
func TestFindFileAcrossRoots(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "server", "internal", "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "server", "internal", "web", "icons.go")
	if err := os.WriteFile(target, []byte("package web\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"server/internal/web/icons.go", "internal/web/icons.go"} {
		got, ok := findFile([]string{"/нет/такой/папки", root}, path)
		if !ok || got != target {
			t.Errorf("%s: нашли %q, ok=%v", path, got, ok)
		}
	}
	if _, ok := findFile([]string{root}, "internal/web/нет.go"); ok {
		t.Error("несуществующий файл считается найденным")
	}
	if _, ok := findFile(nil, "internal/web/icons.go"); ok {
		t.Error("без рабочих папок искать негде, а файл нашёлся")
	}
}
