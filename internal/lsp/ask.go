package lsp

// Вопросы к языковому серверу: оглавление файла, ссылки, объявление,
// диагностика, поиск по проекту. Всё, из чего собирается карточка.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// Open открывает документ. Текст можно передать явно — тогда файл на
// диске не нужен, и так проверяется код, живущий в базе.
func (c *Client) Open(uri, language, text string) error {
	if text == "" && !strings.Contains(uri, "://") {
		raw, err := os.ReadFile(uri)
		if err != nil {
			return err
		}
		text = string(raw)
	}
	return c.Notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": PathToURI(uri), "languageId": language, "version": 1, "text": text,
		},
	})
}

func (c *Client) Close_(uri string) error {
	return c.Notify("textDocument/didClose", map[string]any{
		"textDocument": map[string]any{"uri": PathToURI(uri)},
	})
}

// Symbols — оглавление файла: что в нём объявлено.
func (c *Client) Symbols(ctx context.Context, uri string) ([]Symbol, error) {
	var out []Symbol
	err := c.Call(ctx, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": PathToURI(uri)},
	}, &out)
	return out, err
}

// References — где символ используется. includeDecl — считать ли само
// объявление.
func (c *Client) References(ctx context.Context, uri string, pos Position, includeDecl bool) ([]Location, error) {
	var out []Location
	err := c.Call(ctx, "textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": PathToURI(uri)},
		"position":     pos,
		"context":      map[string]any{"includeDeclaration": includeDecl},
	}, &out)
	return out, err
}

// Definition — где символ объявлен.
func (c *Client) Definition(ctx context.Context, uri string, pos Position) ([]Location, error) {
	var out []Location
	if err := c.Call(ctx, "textDocument/definition", map[string]any{
		"textDocument": map[string]any{"uri": PathToURI(uri)},
		"position":     pos,
	}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Hover — краткое описание символа: сигнатура и док-строка.
func (c *Client) Hover(ctx context.Context, uri string, pos Position) (string, error) {
	var out struct {
		Contents struct {
			Value string `json:"value"`
			Kind  string `json:"kind"`
		} `json:"contents"`
	}
	if err := c.Call(ctx, "textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": PathToURI(uri)},
		"position":     pos,
	}, &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.Contents.Value), nil
}

// WorkspaceSymbols — поиск символа по всему проекту.
func (c *Client) WorkspaceSymbols(ctx context.Context, query string) ([]Location, []string, error) {
	var out []struct {
		Name     string   `json:"name"`
		Kind     int      `json:"kind"`
		Location Location `json:"location"`
	}
	if err := c.Call(ctx, "workspace/symbol", map[string]any{"query": query}, &out); err != nil {
		return nil, nil, err
	}
	var places []Location
	var names []string
	for _, s := range out {
		places = append(places, s.Location)
		names = append(names, s.Name)
	}
	return places, names, nil
}

// Diagnostics — замечания по документу. Сервер присылает их сам, поэтому
// ждём недолго после открытия файла.
// Diagnostics ждёт замечания по документу. Второе значение отличает
// «сервер сказал, что всё чисто» от «сервер не успел ответить» — путать
// это нельзя: во втором случае клиент решит, что кода касаться безопасно.
func (c *Client) Diagnostics(ctx context.Context, uri string, wait time.Duration) ([]Diagnostic, bool) {
	full := PathToURI(uri)
	deadline := time.Now().Add(wait)
	for {
		c.diagsMu.Lock()
		d, ok := c.diags[full]
		c.diagsMu.Unlock()
		if ok {
			return d, true
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// FindSymbol ищет объявление по имени в оглавлении файла, включая
// вложенные. Имя сравнивается терпимо: языковой сервер называет метод
// то `Close`, то `(*Store).Close`, то `Store.Close` — человек же пишет
// просто имя.
func FindSymbol(symbols []Symbol, name string) (Symbol, bool) {
	name = strings.TrimSpace(name)
	for _, s := range symbols {
		if sameName(s.Name, name) {
			return s, true
		}
		if found, ok := FindSymbol(s.Children, name); ok {
			return found, true
		}
	}
	return Symbol{}, false
}

// sameName — тот ли это символ: сравниваем последнюю часть имени, без
// получателя, звёздочек и скобок аргументов.
func sameName(full, want string) bool {
	full = bareName(full)
	want = bareName(want)
	return full == want
}

func bareName(s string) string {
	s = strings.TrimSpace(s)
	// «(*Store).Close» и «(e *Engine) Court» — получатель в скобках
	if strings.HasPrefix(s, "(") {
		if i := strings.IndexByte(s, ')'); i >= 0 {
			s = strings.TrimSpace(strings.TrimPrefix(s[i+1:], "."))
		}
	}
	if i := strings.IndexByte(s, '('); i >= 0 {
		s = s[:i] // отрезаем аргументы: Close(id int64)
	}
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		s = s[i+1:] // Store.Close -> Close
	}
	return strings.TrimSpace(strings.TrimPrefix(s, "*"))
}

// Flatten — плоский список символов с уровнем вложенности.
func Flatten(symbols []Symbol, depth int) []FlatSymbol {
	var out []FlatSymbol
	for _, s := range symbols {
		out = append(out, FlatSymbol{Symbol: s, Depth: depth})
		out = append(out, Flatten(s.Children, depth+1)...)
	}
	return out
}

type FlatSymbol struct {
	Symbol
	Depth int
}

// Line — строка объявления, считая с единицы.
func (f FlatSymbol) Line() int { return f.Selection.Start.Line + 1 }

func (f FlatSymbol) String() string {
	pad := strings.Repeat("  ", f.Depth)
	detail := ""
	if f.Detail != "" {
		detail = " " + f.Detail
	}
	return fmt.Sprintf("%s%s %s%s :%d", pad, f.KindTitle(), f.Name, detail, f.Line())
}
