package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	semalink "github.com/PavluninVladimir/sema-agent/internal/link"
	"github.com/PavluninVladimir/sema-agent/internal/lsp"
)

// fakeSema — провод к Сёме для проверок: отвечает тем, что положили, или
// медлит, или рвётся.
type fakeSema struct {
	text  string
	err   error
	delay time.Duration
	got   map[string]any
	calls int
}

func (f *fakeSema) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	f.calls++
	if m, ok := params.(map[string]any); ok {
		f.got = m
		f.got["_method"] = method
	}
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	raw, _ := json.Marshal(map[string]any{"text": f.text})
	return raw, nil
}

// Карточка приходит с приложением из памяти: раздел «Что здесь известно»,
// одним запросом со всеми местами карточки, и считается как карточка с
// приложением.
func TestCardCarriesKnowledge(t *testing.T) {
	sema := &fakeSema{text: "Что известно про inbox.go:\n- [урок] тут нельзя резать по байтам\n"}
	s := &server{sema: sema}
	// репозиторий — по метке корня: файл лежит в server/internal/api
	root := filepath.Join(t.TempDir(), "server")
	file := filepath.Join(root, "internal", "api", "inbox.go")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(root, "go.mod"), file} {
		if err := os.WriteFile(f, []byte("module x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := lsp.Card{Tier: lsp.TierLSP, Text: "inbox.go — 10 строк\n", Symbols: []string{"buildInbox", "inboxRows"}}
	s.attachKnowledge(&c, file, "Claude Code")
	if !c.WithKnowledge {
		t.Fatal("карточка с приложением не помечена")
	}
	if !strings.Contains(c.Text, knowledgeTitle) || !strings.Contains(c.Text, "нельзя резать по байтам") {
		t.Fatalf("приложения нет в карточке:\n%s", c.Text)
	}
	if sema.calls != 1 {
		t.Fatalf("запросов к памяти %d, ждали один на карточку", sema.calls)
	}
	if sema.got["_method"] != "память.code_knowledge" || sema.got["client"] != "Claude Code" {
		t.Fatalf("не тот вызов: %+v", sema.got)
	}
	places, _ := sema.got["places"].([]placeArg)
	if len(places) != 4 { // папка, файл, два символа
		t.Fatalf("мест в запросе %d: %+v", len(places), places)
	}
	if places[0].Dir != "internal/api" || places[1].File != "internal/api/inbox.go" || places[2].Symbol != "buildInbox" {
		t.Fatalf("места не те: %+v", places)
	}
	if sema.got["repo"] != "server" {
		t.Fatalf("репозиторий не от git-корня: %v", sema.got["repo"])
	}
}

// Провод оборван — карточка приходит сразу, с пометкой, а не через
// таймаут; память не отвечает — пометка по сроку, а не ожидание.
func TestCardWithoutSemaIsImmediate(t *testing.T) {
	c := lsp.Card{Tier: lsp.TierLSP, Text: "x\n"}
	s := &server{sema: &fakeSema{err: semalink.ErrNoLink}}
	start := time.Now()
	s.attachKnowledge(&c, "/tmp/x.go", "Codex")
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("карточка ждала оборванный провод")
	}
	if c.WithKnowledge || !strings.Contains(c.Text, "память недоступна") {
		t.Fatalf("пометки про связь нет:\n%s", c.Text)
	}

	slow := &server{sema: &fakeSema{text: "поздно", delay: 5 * time.Second}, knowledgeWait: 150 * time.Millisecond}
	c = lsp.Card{Tier: lsp.TierLSP, Text: "x\n"}
	start = time.Now()
	slow.attachKnowledge(&c, "/tmp/x.go", "Codex")
	if d := time.Since(start); d > time.Second {
		t.Fatalf("карточка ждала память %s при сроке 150 мс", d)
	}
	if c.WithKnowledge || !strings.Contains(c.Text, "не ответил за") {
		t.Fatalf("пометки про срок нет:\n%s", c.Text)
	}

	// без провода вовсе — тоже пометка, и Сёму никто не спрашивает
	c = lsp.Card{Tier: lsp.TierLSP, Text: "x\n"}
	(&server{}).attachKnowledge(&c, "/tmp/x.go", "Codex")
	if !strings.Contains(c.Text, "без связи с Сёмой") {
		t.Fatalf("пометки нет:\n%s", c.Text)
	}
	// области не выданы — адрес экрана, где выдать
	c = lsp.Card{Tier: lsp.TierLSP, Text: "x\n"}
	(&server{sema: &fakeSema{text: "Области памяти этому агенту в этой папке не выданы: выдать можно на экране «Память» (memory)."}}).attachKnowledge(&c, "/tmp/x.go", "Codex")
	if c.WithKnowledge || !strings.Contains(c.Text, "не выданы") || !strings.Contains(c.Text, "memory") {
		t.Fatalf("пометки про области нет:\n%s", c.Text)
	}
	// Сёма спрашивает карточку сама (клиента нет) — приложения не надо
	c = lsp.Card{Tier: lsp.TierLSP, Text: "x\n"}
	own := &fakeSema{text: "лишнее"}
	(&server{sema: own}).attachKnowledge(&c, "/tmp/x.go", "")
	if own.calls != 0 || strings.Contains(c.Text, knowledgeTitle) {
		t.Fatal("карточку для Сёмы снабдили приложением из её же памяти")
	}
	_ = errors.New
}
