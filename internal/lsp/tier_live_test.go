package lsp

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Живая проверка ярусов ниже lsp на этом репозитории: для yaml языкового
// сервера нет, значит ответ должен прийти ярусом tags или text — и с
// честной пометкой, а не отказом.
func TestTiersBelowLSPLive(t *testing.T) {
	if testing.Short() {
		t.Skip("живая проверка")
	}
	p := NewPool(nil, 3, time.Minute)
	defer p.Close()
	ct, gr := p.ToolsReady()
	t.Logf("ctags: %v, поиск: %v", ct, gr)
	if !gr {
		t.Skip("поиска на машине нет")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	root := serverSource(t)
	c, err := p.Symbols(ctx, root, root+"/config.example.yaml")
	t.Logf("symbols yaml → ярус %s, строк %d, сэкономлено %d, err %v\n%s", c.Tier, c.Lines, c.Saved, err, clip(c.Text, 300))
	if err == nil && c.Tier == TierLSP {
		t.Error("для yaml не может быть яруса lsp")
	}
	imp, err := p.Impact(ctx, root, root+"/config.example.yaml", "EnsureServerCert")
	t.Logf("impact → ярус %s, err %v\n%s", imp.Tier, err, clip(imp.Text, 400))
	if err != nil {
		t.Fatalf("радиус поражения без языкового сервера: %v", err)
	}
	if imp.Tier != TierText || !strings.Contains(imp.Note, "совпадения по слову") {
		t.Errorf("ярус не назван честно: %s / %s", imp.Tier, imp.Note)
	}
	if !strings.Contains(imp.Text, "agent_link.go") {
		t.Errorf("поиск не нашёл очевидного места:\n%s", imp.Text)
	}
	srch, err := p.Search(ctx, root, root+"/config.example.yaml", "EnsureServerCert")
	t.Logf("search → ярус %s, err %v\n%s", srch.Tier, err, clip(srch.Text, 300))
	if err != nil || srch.Tier != TierText {
		t.Errorf("поиск без языкового сервера: ярус %s, err %v", srch.Tier, err)
	}
	dia, err := p.Diagnostics(ctx, root, root+"/config.example.yaml", "")
	t.Logf("diagnostics → ярус %s, note %q, err %v", dia.Tier, dia.Note, err)
	if err == nil {
		t.Error("диагностика без языкового сервера обязана отказать, а не молчать")
	}
}
