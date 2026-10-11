package lsp

// Карточки: то, ради чего всё затевалось. Агент разработки получает
// выжимку с координатами вместо содержимого файла.
//
// Каждая карточка называет ярус, которым добыта: `lsp` — сервер языка
// ответил, `tags` — приблизительно по справочнику символов, `text` —
// только поиск. Клиент, не знающий точности ответа, будет уверенно
// ошибаться.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// Ярусы точности.
const (
	TierLSP  = "lsp"
	TierTags = "tags"
	TierText = "text"
)

// Card — ответ агента: текст для модели, ярус и счёт сэкономленного.
type Card struct {
	Tier  string `json:"tier"`
	Text  string `json:"text"`
	Lines int    `json:"lines"`       // сколько строк в карточке
	Saved int    `json:"saved_lines"` // сколько строк файла не прочитано
	Note  string `json:"note,omitempty"`
	// WithKnowledge — к карточке приложено знание о месте из памяти
	// Сёмы; счёт ведётся отдельно, чтобы видеть, доходит ли оно
	WithKnowledge bool `json:"with_knowledge,omitempty"`
	// Symbols — имена символов карточки: у оглавления все объявленные, у
	// карточки символа один. По ним собирается список мест для памяти —
	// одним запросом на карточку, а не по запросу на символ
	Symbols []string `json:"symbols,omitempty"`
}

// Symbols — оглавление файла вместо его текста.
func (p *Pool) Symbols(ctx context.Context, root, path string) (Card, error) {
	total := countLines(path)
	c, lang, err := p.Get(ctx, root, path)
	if err != nil {
		return p.symbolsBelow(ctx, path, err)
	}
	if err := p.EnsureOpen(c, lang, path, ""); err != nil {
		return p.symbolsBelow(ctx, path, err)
	}
	syms, err := c.Symbols(ctx, path)
	if err != nil {
		return p.symbolsBelow(ctx, path, err)
	}
	flat := Flatten(syms, 0)
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %d строк, объявлено %d\n", path, total, len(flat))
	names := make([]string, 0, len(flat))
	seen := map[string]bool{}
	for _, f := range flat {
		b.WriteString(f.String() + "\n")
		if f.Name != "" && !seen[f.Name] {
			seen[f.Name] = true
			names = append(names, f.Name)
		}
	}
	return Card{Tier: TierLSP, Text: b.String(), Lines: len(flat) + 1, Saved: max0(total - len(flat) - 1), Symbols: names}, nil
}

// Context — карточка символа: где объявлен, что это, кто зовёт.
func (p *Pool) Context(ctx context.Context, root, path, name string) (Card, error) {
	total := countLines(path)
	c, lang, err := p.Get(ctx, root, path)
	if err != nil {
		return p.contextBelow(ctx, root, path, name, err)
	}
	if err := p.EnsureOpen(c, lang, path, ""); err != nil {
		return p.contextBelow(ctx, root, path, name, err)
	}
	syms, err := c.Symbols(ctx, path)
	if err != nil {
		return p.contextBelow(ctx, root, path, name, err)
	}
	s, ok := FindSymbol(syms, name)
	if !ok {
		return Card{Tier: TierLSP, Note: fmt.Sprintf("символа %q в %s нет", name, path)},
			fmt.Errorf("символа %q в файле нет", name)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n%s\n", s.KindTitle(), s.Name, Where(PathToURI(path), s.Selection))
	if hover, err := c.Hover(ctx, path, s.Selection.Start); err == nil && hover != "" {
		b.WriteString(clip(hover, 800) + "\n")
	}
	refs, err := c.References(ctx, path, s.Selection.Start, false)
	if err == nil {
		fmt.Fprintf(&b, "\nЗовут в %d местах:\n", len(refs))
		for i, r := range refs {
			if i >= 12 {
				fmt.Fprintf(&b, "  … и ещё %d\n", len(refs)-i)
				break
			}
			b.WriteString("  " + Where(r.URI, r.Range) + "\n")
		}
	}
	size := s.Range.End.Line - s.Range.Start.Line + 1
	lines := strings.Count(b.String(), "\n")
	return Card{Tier: TierLSP, Text: b.String(), Lines: lines, Saved: max0(total - lines),
		Note: fmt.Sprintf("сам символ занимает %d строк", size), Symbols: []string{name}}, nil
}

// Impact — радиус поражения: что сломается, если это менять.
func (p *Pool) Impact(ctx context.Context, root, path, name string) (Card, error) {
	c, lang, err := p.Get(ctx, root, path)
	if err != nil {
		return p.impactBelow(ctx, root, name, err)
	}
	if err := p.EnsureOpen(c, lang, path, ""); err != nil {
		return p.impactBelow(ctx, root, name, err)
	}
	syms, err := c.Symbols(ctx, path)
	if err != nil {
		return p.impactBelow(ctx, root, name, err)
	}
	s, ok := FindSymbol(syms, name)
	if !ok {
		return Card{Tier: TierLSP, Note: "символа нет"}, fmt.Errorf("символа %q в файле нет", name)
	}
	refs, err := c.References(ctx, path, s.Selection.Start, false)
	if err != nil {
		return p.impactBelow(ctx, root, name, err)
	}
	files := map[string]int{}
	tests := map[string]int{}
	for _, r := range refs {
		f := URIToPath(r.URI)
		files[f]++
		if strings.HasSuffix(f, "_test.go") || strings.Contains(f, "test") {
			tests[f]++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Правка %s затронет %d мест в %d файлах\n", name, len(refs), len(files))
	for f, n := range files {
		mark := ""
		if tests[f] > 0 {
			mark = "  (тест)"
		}
		fmt.Fprintf(&b, "  %s — %d%s\n", f, n, mark)
	}
	if len(tests) == 0 {
		b.WriteString("\nТестов, которые это трогают, не нашлось — правка не прикрыта.\n")
	}
	lines := strings.Count(b.String(), "\n")
	return Card{Tier: TierLSP, Text: b.String(), Lines: lines}, nil
}

// Diagnostics — что сломано сейчас. Текст можно передать явно: тогда
// проверяется код, которого нет на диске.
func (p *Pool) Diagnostics(ctx context.Context, root, path, text string) (Card, error) {
	c, lang, err := p.Get(ctx, root, path)
	if err != nil {
		// ярусов ниже у диагностики нет: ни ctags, ни поиск не скажут,
		// что код сломан. Молчание — не «замечаний нет».
		return Card{Tier: TierText, Note: "языкового сервера для этого языка нет: судить о коде нечем — " + err.Error()}, err
	}
	if err := c.Open(path, lang.Name, text); err != nil {
		return Card{Tier: TierText, Note: err.Error()}, err
	}
	diags, answered := c.Diagnostics(ctx, path, 5*time.Second)
	var b strings.Builder
	switch {
	case !answered:
		// молчание сервера — не чистый код: так и говорим
		return Card{Tier: TierText, Text: "Языковой сервер не ответил вовремя: судить о коде нечем.\n",
			Note: "замечаний не получено, это не значит, что их нет"}, nil
	case len(diags) == 0:
		b.WriteString("Замечаний нет.\n")
	}
	for _, d := range diags {
		fmt.Fprintf(&b, "%s %s:%d — %s\n", SeverityTitle(d.Severity), path, d.Range.Start.Line+1, d.Message)
	}
	return Card{Tier: TierLSP, Text: b.String(), Lines: strings.Count(b.String(), "\n")}, nil
}

// Definition — где объявлено имя под курсором.
func (p *Pool) Definition(ctx context.Context, root, path string, line, col int) (Card, error) {
	c, lang, err := p.Get(ctx, root, path)
	if err != nil {
		return p.definitionBelow(ctx, root, path, line, col, err)
	}
	if err := p.EnsureOpen(c, lang, path, ""); err != nil {
		return p.definitionBelow(ctx, root, path, line, col, err)
	}
	places, err := c.Definition(ctx, path, Position{Line: line - 1, Character: col})
	if err != nil {
		return p.definitionBelow(ctx, root, path, line, col, err)
	}
	var b strings.Builder
	for _, l := range places {
		b.WriteString(Where(l.URI, l.Range) + "\n")
	}
	if len(places) == 0 {
		b.WriteString("Объявление не найдено.\n")
	}
	return Card{Tier: TierLSP, Text: b.String(), Lines: len(places)}, nil
}

// Search — поиск символа по проекту.
func (p *Pool) Search(ctx context.Context, root, anyFile, query string) (Card, error) {
	c, _, err := p.Get(ctx, root, anyFile)
	if err != nil {
		return p.searchBelow(ctx, root, query, err)
	}
	places, names, err := c.WorkspaceSymbols(ctx, query)
	if err != nil {
		return p.searchBelow(ctx, root, query, err)
	}
	var b strings.Builder
	for i := range places {
		if i >= 40 {
			fmt.Fprintf(&b, "… и ещё %d\n", len(places)-i)
			break
		}
		fmt.Fprintf(&b, "%s — %s\n", names[i], Where(places[i].URI, places[i].Range))
	}
	if len(places) == 0 {
		b.WriteString("Ничего не нашлось.\n")
	}
	return Card{Tier: TierLSP, Text: b.String(), Lines: strings.Count(b.String(), "\n")}, nil
}

func countLines(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "\n") + 1
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// Ниже — спуск по ярусам. Языковой сервер не поднялся или не ответил:
// вместо отказа отвечаем тем, что есть, и говорим, чем именно.

func (p *Pool) symbolsBelow(ctx context.Context, path string, why error) (Card, error) {
	if c, ok := p.symbolsTags(ctx, path); ok {
		return c, nil
	}
	if c, ok := p.symbolsText(path); ok {
		return c, nil
	}
	return Card{Tier: TierText, Note: why.Error()}, why
}

func (p *Pool) contextBelow(ctx context.Context, root, path, name string, why error) (Card, error) {
	if c, ok := p.contextTags(ctx, root, path, name); ok {
		return c, nil
	}
	return Card{Tier: TierText, Note: why.Error()}, why
}

func (p *Pool) impactBelow(ctx context.Context, root, name string, why error) (Card, error) {
	if c, ok := p.impactText(ctx, root, name); ok {
		return c, nil
	}
	return Card{Tier: TierText, Note: why.Error()}, why
}

func (p *Pool) definitionBelow(ctx context.Context, root, path string, line, col int, why error) (Card, error) {
	if c, ok := p.definitionText(ctx, root, path, line, col); ok {
		return c, nil
	}
	return Card{Tier: TierText, Note: why.Error()}, why
}

func (p *Pool) searchBelow(ctx context.Context, root, query string, why error) (Card, error) {
	if c, ok := p.searchText(ctx, root, query); ok {
		return c, nil
	}
	return Card{Tier: TierText, Note: why.Error()}, why
}

// HasSymbol — жив ли ещё символ в этом файле. Вопрос задаёт Сёма, когда
// проверяет координаты своих записей памяти: «что здесь уже ломалось»
// должно указывать на живое место.
//
// Молчание не ответ: если языкового сервера нет и ctags не установлен,
// возвращается «не знаю» (known = false) — по нему координату не снимают.
// Иначе одна ненастроенная машина стёрла бы всю разметку памяти.
func (p *Pool) HasSymbol(ctx context.Context, root, path, name string) (found, known bool, tier string) {
	if name == "" {
		return false, false, ""
	}
	c, lang, err := p.Get(ctx, root, path)
	if err == nil {
		if err = p.EnsureOpen(c, lang, path, ""); err == nil {
			if syms, err := c.Symbols(ctx, path); err == nil {
				for _, f := range Flatten(syms, 0) {
					if f.Name == name {
						return true, true, TierLSP
					}
				}
				return false, true, TierLSP
			}
		}
	}
	// ярусом ниже: оглавление по ctags. Он честно перечисляет объявления
	// файла, и отсутствия имени в нём достаточно
	if tags, err := p.ctagsFile(ctx, path); err == nil && len(tags) > 0 {
		for _, t := range tags {
			if t.Name == name {
				return true, true, TierTags
			}
		}
		return false, true, TierTags
	}
	return false, false, ""
}
