package main

// Принципы проекта из CLAUDE.md зонтика — в память Сёмы ссылками.
//
// Абзацы «Архитектуры» и «Правил проекта» называют пути в коде
// (`api/ui.go`, `engine/queue.go`, `internal/web`). Житель видит файл и
// репозитории, Сёма — нет: разбор здесь, хранение там. В память едет не
// абзац, а одна строка — первое предложение, заголовок абзаца и отпечаток
// текста; по отпечатку Сёма решает, что нового, что переписано, что
// исчезло. Разбор запускается при старте сессии агента разработки и
// только когда файл изменился: та же пачка дважды Сёме ничего не даёт.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// principle — один путь одного абзаца.
type principle struct {
	Repo    string `json:"repo"`
	Dir     string `json:"dir,omitempty"`
	File    string `json:"file,omitempty"`
	Heading string `json:"heading"`
	Hash    string `json:"hash"`
	Text    string `json:"text"`
}

// reBoldLead — абзац с заголовком жирным в начале: «**Очередь фоновой
// работы** (…). Всё, что…» или пункт списка «- **Сначала дизайн.** …».
var reBoldLead = regexp.MustCompile(`^\s*(?:[-*]\s+)?\*\*([^*]+)\*\*`)

// reSpan — кусок в обратных кавычках.
var reSpan = regexp.MustCompile("`([^`\n]+)`")

// rePathLike — похоже на путь: слова через слэш, у файла расширение.
var rePathLike = regexp.MustCompile(`^[\w.+-]+(?:/[\w.+-]+)*/?$`)

// pathExts — расширения файлов, которые считаем кодом или документом.
var pathExts = []string{".go", ".js", ".css", ".md", ".swift", ".html", ".yaml", ".yml", ".txt"}

// resolver — где лежит путь из абзаца: репозиторий и путь от его корня.
// ok — путь существует на диске; выдуманная координата хуже отсутствия.
type resolver func(p string) (repo, dir, file string, ok bool)

// parsePrinciples — привязки из текста CLAUDE.md.
func parsePrinciples(text string, resolve resolver) []principle {
	var out []principle
	seen := map[string]bool{}
	for _, para := range splitParagraphs(text) {
		m := reBoldLead.FindStringSubmatch(para)
		if m == nil {
			continue
		}
		heading := strings.TrimRight(strings.TrimSpace(m[1]), ".:;,")
		if heading == "" {
			continue
		}
		hash := paragraphHash(para)
		lead := firstSentence(para)
		add := func(repo, dir, file string) {
			key := heading + "|" + repo + "|" + dir + "|" + file
			if seen[key] {
				return
			}
			seen[key] = true
			out = append(out, principle{Repo: repo, Dir: dir, File: file, Heading: heading, Hash: hash, Text: lead})
		}
		// два прохода: сначала пути с папкой, потом голые имена файлов —
		// «Экраны собираются по файлам: `inbox.go`, `files.go`» ищутся в
		// папках, которые тот же абзац уже назвал (`api/ui.go` → `api/`)
		var bare []string
		var dirsSeen []string
		for _, span := range reSpan.FindAllStringSubmatch(para, -1) {
			p := strings.TrimSpace(span[1])
			if !looksLikePath(p) {
				continue
			}
			if !strings.Contains(p, "/") {
				bare = append(bare, p)
				continue
			}
			repo, dir, file, ok := resolve(p)
			if !ok {
				continue
			}
			add(repo, dir, file)
			if d := dir; d == "" {
				d = path.Dir(file)
				dirsSeen = appendUnique(dirsSeen, repo+"/"+d)
			} else {
				dirsSeen = appendUnique(dirsSeen, repo+"/"+d)
			}
		}
		for _, name := range bare {
			if repo, dir, file, ok := resolve(name); ok {
				add(repo, dir, file)
				continue
			}
			for _, rd := range dirsSeen {
				repo, d, _ := strings.Cut(rd, "/")
				if r2, dir, file, ok := resolve(repo + "/" + d + "/" + name); ok {
					add(r2, dir, file)
					break
				}
			}
		}
	}
	return out
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// splitParagraphs — абзацы по пустым строкам; строки абзаца в одну.
func splitParagraphs(text string) []string {
	var out []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.Join(cur, " "))
			cur = nil
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		// заголовок раздела и таблица — не абзацы с принципами
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "|") || strings.HasPrefix(line, "```") {
			flush()
			continue
		}
		cur = append(cur, strings.TrimSpace(line))
	}
	flush()
	return out
}

// looksLikePath — путь к файлу или папке, а не слово в кавычках.
func looksLikePath(p string) bool {
	if !rePathLike.MatchString(p) || strings.Contains(p, "..") {
		return false
	}
	if strings.Contains(p, "/") {
		return true
	}
	for _, ext := range pathExts {
		if strings.HasSuffix(p, ext) && len(p) > len(ext) {
			return true
		}
	}
	return false
}

// paragraphHash — отпечаток абзаца без лишних пробелов: перенос строки в
// другом месте — не другой абзац.
func paragraphHash(para string) string {
	norm := strings.Join(strings.Fields(para), " ")
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:8])
}

// firstSentence — одна строка итога: первое предложение без разметки.
func firstSentence(para string) string {
	// заголовок абзаца — отдельное поле, в строке итога он лишний; скобки с
	// путями сразу за ним («(`store/queue.go`, …).») — тоже
	s := reBoldLead.ReplaceAllString(para, "")
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "(") {
		if i := strings.Index(s, ")"); i > 0 {
			s = s[i+1:]
		}
	}
	s = strings.NewReplacer("**", "", "`", "").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	// маркер пункта списка и знаки после заголовка — разметка, не текст
	s = strings.TrimLeft(s, "-*.:;, ")
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i+1]
	}
	if r := []rune(s); len(r) > 300 {
		s = string(r[:297]) + "…"
	}
	return strings.TrimSpace(s)
}

// umbrellaResolver — где искать пути из CLAUDE.md зонтика: сам зонтик,
// репозитории под ним (с их `internal/`: абзацы пишут `engine/queue.go`,
// а лежит он в `server/internal/engine/`) и соседний репозиторий жителя.
func umbrellaResolver(root string) resolver {
	var repos []string
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			if e.IsDir() && hasDir(filepath.Join(root, e.Name()), ".git") {
				repos = append(repos, filepath.Join(root, e.Name()))
			}
		}
	}
	if sib := filepath.Join(filepath.Dir(root), "semaagent"); hasDir(sib, ".git") {
		repos = append(repos, sib)
	}
	// репозитории раньше зонтика: карточка называет место от git-корня
	// («server», «internal/api/ui.go»), и привязка должна совпасть с ней,
	// а не лечь на зонтик путём «server/internal/api/ui.go»
	var bases [][2]string
	for _, r := range repos {
		bases = append(bases, [2]string{r, ""}, [2]string{r, "internal"})
	}
	bases = append(bases, [2]string{root, ""})
	return func(p string) (repo, dir, file string, ok bool) {
		wantDir := strings.HasSuffix(p, "/")
		p = strings.Trim(p, "/")
		// путь с именем репозитория впереди («server/internal/api/ui.go»)
		// — внутрь этого репозитория, без приставки
		if first, rest, cut := strings.Cut(p, "/"); cut {
			for _, r := range repos {
				if filepath.Base(r) == first {
					if sub := tryPlace(r, "", rest, wantDir); sub.ok {
						return sub.repo, sub.dir, sub.file, true
					}
				}
			}
		}
		for _, b := range bases {
			if got := tryPlace(b[0], b[1], p, wantDir); got.ok {
				return got.repo, got.dir, got.file, true
			}
		}
		return "", "", "", false
	}
}

// resolved — что нашлось по пути в одном репозитории.
type resolved struct {
	repo, dir, file string
	ok              bool
}

// tryPlace — есть ли путь p в репозитории base (возможно, под подпапкой
// sub); репозиторий — имя его корня, путь — от корня.
func tryPlace(base, sub, p string, wantDir bool) resolved {
	full := filepath.Join(base, sub, filepath.FromSlash(p))
	st, err := os.Stat(full)
	if err != nil {
		return resolved{}
	}
	rel, err := filepath.Rel(base, full)
	if err != nil {
		return resolved{}
	}
	rel = filepath.ToSlash(rel)
	repo := filepath.Base(base)
	if st.IsDir() {
		return resolved{repo: repo, dir: rel, ok: true}
	}
	if wantDir {
		return resolved{}
	}
	return resolved{repo: repo, file: rel, ok: true}
}

// principlesKeeper — помнит, какой CLAUDE.md уже разобран, чтобы не
// слать Сёме одну и ту же пачку на каждом старте сессии.
type principlesKeeper struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// principlesMaybe — разобрать CLAUDE.md корня работы и отправить Сёме,
// если файл изменился с прошлого раза. В фоне: хук старта сессии ждать
// этого не должен.
func (s *server) principlesMaybe(root, client string) {
	if s.sema == nil || root == "" {
		return
	}
	file := filepath.Join(root, "CLAUDE.md")
	st, err := os.Stat(file)
	if err != nil {
		return
	}
	s.principles.mu.Lock()
	if s.principles.seen == nil {
		s.principles.seen = map[string]time.Time{}
	}
	if last, ok := s.principles.seen[file]; ok && last.Equal(st.ModTime()) {
		s.principles.mu.Unlock()
		return
	}
	s.principles.seen[file] = st.ModTime()
	s.principles.mu.Unlock()
	go s.sendPrinciples(root, file, client)
}

func (s *server) sendPrinciples(root, file, client string) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return
	}
	items := parsePrinciples(string(raw), umbrellaResolver(root))
	if len(items) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reply, err := s.sema.Call(ctx, "память.principles", map[string]any{
		"path": root, "client": client, "items": items})
	if err != nil {
		// не вышло — в следующий раз попробуем снова: отпечаток времени
		// снимаем, иначе пачка не уйдёт до правки файла
		s.principles.mu.Lock()
		delete(s.principles.seen, file)
		s.principles.mu.Unlock()
		log.Printf("принципы из %s не дошли до Сёмы: %v", file, err)
		return
	}
	log.Printf("принципы из %s: %d привязок, Сёма: %s", file, len(items), strings.TrimSpace(string(reply)))
}
