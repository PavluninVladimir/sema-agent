package lsp

// Ярусы ниже языкового сервера. Rust, Vue, конфиги — на машине не всегда
// стоит нужный сервер, а вопрос всё равно задан. Тогда агент отвечает
// приблизительно, но **честно**: ярус `tags` — справочник символов
// universal-ctags, ярус `text` — ripgrep. Клиент, не знающий точности
// ответа, будет уверенно ошибаться, поэтому ярус стоит в каждой карточке.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Tools — чем добывать ярусы ниже. Правится настройкой, а не кодом:
// завтра бинарник переименуют, и это не повод пересобирать агента.
type Tools struct {
	Ctags []string // universal-ctags
	Grep  []string // ripgrep, а без него — обычный grep
}

func DefaultTools() Tools {
	t := Tools{Grep: []string{"rg"}}
	if !have(t.Grep) {
		t.Grep = []string{"grep"}
	}
	// На маке в системе лежит ctags из Xcode — это не universal-ctags, он
	// не понимает ни --excmd, ни поля. Проверяем по ответу, а не по
	// наличию бинарника, иначе ярус tags молча ломается.
	for _, name := range []string{"ctags", "uctags", "universal-ctags"} {
		if universalCtags(name) {
			t.Ctags = []string{name}
			break
		}
	}
	return t
}

// universalCtags — это тот самый ctags или однофамилец.
func universalCtags(name string) bool {
	if !have([]string{name}) {
		return false
	}
	out, err := exec.Command(name, "--version").Output()
	return err == nil && strings.Contains(strings.ToLower(string(out)), "universal ctags")
}

func have(cmd []string) bool {
	if len(cmd) == 0 {
		return false
	}
	_, err := exec.LookPath(cmd[0])
	return err == nil
}

// SetTools — заменить утилиты ярусов (из настроек агента).
func (p *Pool) SetTools(t Tools) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tools = t
}

func (p *Pool) toolset() Tools {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tools
}

// ToolsReady — что из ярусов ниже есть на машине: экрану видно, чем
// агент сможет ответить, когда языкового сервера нет.
func (p *Pool) ToolsReady() (ctags, grep bool) {
	t := p.toolset()
	return have(t.Ctags), have(t.Grep)
}

// tagLine — одна строка справочника символов.
type tagLine struct {
	Name string
	Kind string
	Line int
}

// ctagsFile — символы одного файла. Пустой список без ошибки означает
// «утилита есть, а символов не нашла».
func (p *Pool) ctagsFile(ctx context.Context, path string) ([]tagLine, error) {
	t := p.toolset()
	if !have(t.Ctags) {
		return nil, fmt.Errorf("universal-ctags не установлен")
	}
	args := append([]string{}, t.Ctags[1:]...)
	args = append(args, "-f", "-", "--excmd=number", "--fields=+nK", "--sort=no", path)
	out, err := exec.CommandContext(ctx, t.Ctags[0], args...).Output()
	if err != nil {
		return nil, fmt.Errorf("ctags не ответил: %w", err)
	}
	var list []tagLine
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "!_TAG") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		tl := tagLine{Name: parts[0]}
		for _, f := range parts[2:] {
			switch {
			case strings.HasPrefix(f, "line:"):
				tl.Line, _ = strconv.Atoi(strings.TrimPrefix(f, "line:"))
			case strings.HasPrefix(f, "kind:"):
				tl.Kind = strings.TrimPrefix(f, "kind:")
			case tl.Kind == "" && len(f) <= 2 && f != "":
				tl.Kind = f // старый формат: голая буква вида
			}
		}
		if tl.Line == 0 {
			// номер строки бывает в поле-образце: `12;"`
			if n, err := strconv.Atoi(strings.TrimSuffix(parts[2], `;"`)); err == nil {
				tl.Line = n
			}
		}
		list = append(list, tl)
	}
	return list, nil
}

// place — место в коде: файл и строка, без содержимого.
type place struct {
	File string
	Line int
	Text string
}

// grepWord — где встречается слово. Ищем целым словом: подстрока даёт
// шум, из-за которого радиус поражения врёт в большую сторону.
func (p *Pool) grepWord(ctx context.Context, root, word string, limit int) ([]place, error) {
	t := p.toolset()
	if !have(t.Grep) {
		return nil, fmt.Errorf("ни ripgrep, ни grep не установлены")
	}
	base := filepath.Base(t.Grep[0])
	args := append([]string{}, t.Grep[1:]...)
	if base == "rg" {
		args = append(args, "--line-number", "--no-heading", "--color=never",
			"--fixed-strings", "--word-regexp", "--", word, root)
	} else {
		args = append(args, "-rnwF", "--binary-files=without-match", "--", word, root)
	}
	out, err := exec.CommandContext(ctx, t.Grep[0], args...).Output()
	// код 1 — «ничего не нашлось», это ответ; всё остальное — поломка, и
	// прятать её под «ничего не нашлось» нельзя
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			return nil, fmt.Errorf("поиск не отработал: %w", err)
		}
	}
	var list []place
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		file, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		num, text, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		list = append(list, place{File: file, Line: n, Text: strings.TrimSpace(text)})
		if limit > 0 && len(list) >= limit {
			break
		}
	}
	return list, nil
}

// declRe — на что похоже объявление на ярусе text. Языки разные, поэтому
// список общий: лучше показать несколько лишних строк, чем ни одной.
var declRe = regexp.MustCompile(`^\s*(export\s+)?(pub\s+)?(async\s+)?(func|function|fn|def|class|struct|interface|type|impl|trait|enum|const|let|var|module|component)\b`)

// symbolsTags — оглавление файла без языкового сервера.
func (p *Pool) symbolsTags(ctx context.Context, path string) (Card, bool) {
	tags, err := p.ctagsFile(ctx, path)
	if err != nil || len(tags) == 0 {
		return Card{}, false
	}
	total := countLines(path)
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %d строк, объявлено %d\n", path, total, len(tags))
	for _, t := range tags {
		kind := t.Kind
		if kind == "" {
			kind = "символ"
		}
		fmt.Fprintf(&b, "  %s %s — строка %d\n", kind, t.Name, t.Line)
	}
	lines := len(tags) + 1
	return Card{Tier: TierTags, Text: b.String(), Lines: lines, Saved: max0(total - lines),
		Note: "языкового сервера для этого языка нет: список по справочнику символов, без типов и связей"}, true
}

// symbolsText — последний ярус: строки, похожие на объявления.
func (p *Pool) symbolsText(path string) (Card, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Card{}, false
	}
	total := 0
	var b strings.Builder
	fmt.Fprintf(&b, "%s — строки, похожие на объявления\n", path)
	found := 0
	for i, line := range strings.Split(string(raw), "\n") {
		total++
		if declRe.MatchString(line) {
			found++
			if found <= 200 {
				fmt.Fprintf(&b, "  %d: %s\n", i+1, clip(strings.TrimSpace(line), 120))
			}
		}
	}
	if found == 0 {
		return Card{}, false
	}
	lines := found + 1
	return Card{Tier: TierText, Text: b.String(), Lines: lines, Saved: max0(total - lines),
		Note: "ни языкового сервера, ни справочника символов: это догадка по тексту"}, true
}

// impactText — радиус поражения поиском по слову.
func (p *Pool) impactText(ctx context.Context, root, name string) (Card, bool) {
	places, err := p.grepWord(ctx, root, name, 2000)
	if err != nil || len(places) == 0 {
		return Card{}, false
	}
	files := map[string]int{}
	tests := map[string]bool{}
	for _, pl := range places {
		files[pl.File]++
		if isTestPath(pl.File) {
			tests[pl.File] = true
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Правка %s затронет %d мест в %d файлах\n", name, len(places), len(files))
	for _, f := range sortedKeys(files) {
		mark := ""
		if tests[f] {
			mark = "  (тест)"
		}
		fmt.Fprintf(&b, "  %s — %d%s\n", f, files[f], mark)
	}
	if len(tests) == 0 {
		b.WriteString("\nТестов, которые это трогают, не нашлось — правка не прикрыта.\n")
	}
	return Card{Tier: TierText, Text: b.String(), Lines: strings.Count(b.String(), "\n"),
		Note: "языкового сервера нет: это совпадения по слову, а не ссылки — однофамильцы попадут сюда же"}, true
}

// searchText — поиск мест по слову.
func (p *Pool) searchText(ctx context.Context, root, query string) (Card, bool) {
	places, err := p.grepWord(ctx, root, query, 200)
	if err != nil {
		return Card{}, false
	}
	var b strings.Builder
	if len(places) == 0 {
		b.WriteString("Ничего не нашлось.\n")
	}
	for i, pl := range places {
		if i >= 40 {
			fmt.Fprintf(&b, "… и ещё %d\n", len(places)-i)
			break
		}
		fmt.Fprintf(&b, "%s:%d — %s\n", pl.File, pl.Line, clip(pl.Text, 120))
	}
	return Card{Tier: TierText, Text: b.String(), Lines: strings.Count(b.String(), "\n"),
		Note: "поиск по тексту, а не по символам"}, true
}

// contextTags — карточка символа без языкового сервера: где объявлен по
// справочнику, где встречается — поиском.
func (p *Pool) contextTags(ctx context.Context, root, path, name string) (Card, bool) {
	var b strings.Builder
	tier := TierText
	if tags, err := p.ctagsFile(ctx, path); err == nil {
		for _, t := range tags {
			if sameTagName(t.Name, name) {
				kind := t.Kind
				if kind == "" {
					kind = "символ"
				}
				fmt.Fprintf(&b, "%s %s\n%s:%d\n", kind, t.Name, path, t.Line)
				tier = TierTags
				break
			}
		}
	}
	places, err := p.grepWord(ctx, root, name, 500)
	if err != nil && b.Len() == 0 {
		return Card{}, false
	}
	if b.Len() == 0 {
		fmt.Fprintf(&b, "%s — объявление не найдено, вот где встречается\n", name)
	}
	fmt.Fprintf(&b, "\nВстречается в %d местах:\n", len(places))
	for i, pl := range places {
		if i >= 12 {
			fmt.Fprintf(&b, "  … и ещё %d\n", len(places)-i)
			break
		}
		fmt.Fprintf(&b, "  %s:%d\n", pl.File, pl.Line)
	}
	note := "языкового сервера нет: без типов, встречи считаны по слову"
	return Card{Tier: tier, Text: b.String(), Lines: strings.Count(b.String(), "\n"), Note: note}, true
}

// definitionText — где объявлено слово, стоящее в этом месте файла.
func (p *Pool) definitionText(ctx context.Context, root, path string, line, col int) (Card, bool) {
	word := wordAt(path, line, col)
	if word == "" {
		return Card{}, false
	}
	places, err := p.grepWord(ctx, root, word, 500)
	if err != nil {
		return Card{}, false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — похоже на объявление:\n", word)
	found := 0
	for _, pl := range places {
		if !declRe.MatchString(pl.Text) {
			continue
		}
		found++
		if found <= 10 {
			fmt.Fprintf(&b, "  %s:%d — %s\n", pl.File, pl.Line, clip(pl.Text, 120))
		}
	}
	if found == 0 {
		fmt.Fprintf(&b, "  не нашлось; всего встреч слова: %d\n", len(places))
	}
	return Card{Tier: TierText, Text: b.String(), Lines: strings.Count(b.String(), "\n"),
		Note: "языкового сервера нет: объявление угадано по тексту строки"}, true
}

// wordAt — слово в файле на строке (с единицы) и столбце (с нуля).
func wordAt(path string, line, col int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	rows := strings.Split(string(raw), "\n")
	if line < 1 || line > len(rows) {
		return ""
	}
	r := []rune(rows[line-1])
	if len(r) == 0 {
		return "" // пустая строка: слова здесь нет, а не «возьмём нулевой символ»
	}
	if col < 0 || col >= len(r) {
		col = 0
	}
	isWord := func(c rune) bool {
		return c == '_' || c > 127 || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
	}
	if !isWord(r[col]) {
		return ""
	}
	start, end := col, col
	for start > 0 && isWord(r[start-1]) {
		start--
	}
	for end < len(r)-1 && isWord(r[end+1]) {
		end++
	}
	return string(r[start : end+1])
}

// sameTagName — ctags пишет имя как есть, а спрашивают часто с приёмником.
func sameTagName(tag, want string) bool {
	if tag == want {
		return true
	}
	if i := strings.LastIndex(want, "."); i >= 0 {
		return tag == want[i+1:]
	}
	return false
}

func isTestPath(f string) bool {
	base := strings.ToLower(filepath.Base(f))
	return strings.HasSuffix(base, "_test.go") || strings.Contains(base, "test") || strings.Contains(base, "spec")
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
