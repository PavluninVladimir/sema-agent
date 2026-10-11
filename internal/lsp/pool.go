package lsp

// Пул языковых серверов: один сервер на пару «репозиторий + язык».
// Поднимается при первом вопросе, останавливается по бездействию —
// rust-analyzer на большом проекте стоит сотни мегабайт, держать его
// вечно нельзя.

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Lang — как звать сервер для языка и как язык называется в протоколе.
type Lang struct {
	Name    string   // go, rust, typescript…
	Command []string // чем запускать
	Exts    []string // расширения файлов
	// Init — initializationOptions сервера. Пути внутри проекта пишутся
	// шаблоном {root}: он подставляется в момент подъёма.
	Init map[string]any
}

// Registry — справочник языков. Правится настройкой, а не кодом: завтра
// вендор переименует бинарник, и это не повод пересобирать агента.
type Registry struct {
	langs []Lang
}

// DefaultRegistry — то, что обычно стоит на машине разработчика.
func DefaultRegistry() *Registry {
	return &Registry{langs: []Lang{
		{Name: "go", Command: []string{"gopls"}, Exts: []string{".go"}},
		{Name: "rust", Command: []string{"rust-analyzer"}, Exts: []string{".rs"}},
		{Name: "swift", Command: []string{"sourcekit-lsp"}, Exts: []string{".swift"}},
		{Name: "typescript", Command: []string{"typescript-language-server", "--stdio"},
			Exts: []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}},
		{Name: "python", Command: []string{"pyright-langserver", "--stdio"}, Exts: []string{".py"}},
		{Name: "vue", Command: []string{"vue-language-server", "--stdio"}, Exts: []string{".vue"},
			// Volar без пути к TypeScript поднимается и молчит: ни
			// символов, ни диагностики, ни ошибки
			Init: map[string]any{"typescript": map[string]any{"tsdk": "{root}/node_modules/typescript/lib"}}},
	}}
}

// Add — добавить или заменить язык (из настроек агента).
func (r *Registry) Add(l Lang) {
	for i := range r.langs {
		if r.langs[i].Name == l.Name {
			r.langs[i] = l
			return
		}
	}
	r.langs = append(r.langs, l)
}

// For — язык по имени файла; второе значение false — такого не знаем.
func (r *Registry) For(path string) (Lang, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	for _, l := range r.langs {
		for _, e := range l.Exts {
			if e == ext {
				return l, true
			}
		}
	}
	return Lang{}, false
}

// Available — установлен ли сервер этого языка на машине.
func (l Lang) Available() bool {
	if len(l.Command) == 0 {
		return false
	}
	_, err := exec.LookPath(l.Command[0])
	return err == nil
}

// Langs — весь справочник: экрану видно, что есть, а чего нет.
func (r *Registry) Langs() []Lang { return append([]Lang(nil), r.langs...) }

// Pool — живые серверы.
type Pool struct {
	reg      *Registry
	mu       sync.Mutex
	servers  map[string]*Client // ключ: корень + язык
	max      int
	idle     time.Duration
	opened   map[string]bool        // какие документы уже открыты
	starting map[string]*sync.Mutex // замки на подъём сервера по ключу
	tools    Tools                  // ярусы ниже: справочник символов и поиск
}

func NewPool(reg *Registry, max int, idle time.Duration) *Pool {
	if reg == nil {
		reg = DefaultRegistry()
	}
	if max <= 0 {
		max = 3
	}
	if idle <= 0 {
		idle = 20 * time.Minute
	}
	p := &Pool{reg: reg, servers: map[string]*Client{}, max: max, idle: idle,
		opened: map[string]bool{}, starting: map[string]*sync.Mutex{}, tools: DefaultTools()}
	go p.sweep()
	return p
}

func (p *Pool) Registry() *Registry { return p.reg }

// Get — сервер для файла; поднимает, если ещё не поднят.
func (p *Pool) Get(ctx context.Context, root, path string) (*Client, Lang, error) {
	lang, ok := p.reg.For(path)
	if !ok {
		return nil, Lang{}, fmt.Errorf("язык файла %q не в справочнике", filepath.Base(path))
	}
	if !lang.Available() {
		return nil, lang, fmt.Errorf("языковой сервер %q не установлен", lang.Command[0])
	}
	key := root + "\n" + lang.Name

	// замок на ключ: два одновременных первых вопроса по одному
	// репозиторию иначе поднимут два сервера, и один осиротеет —
	// незаметно съедая сотни мегабайт
	p.mu.Lock()
	lock, ok := p.starting[key]
	if !ok {
		lock = &sync.Mutex{}
		p.starting[key] = lock
	}
	p.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()

	p.mu.Lock()
	if c, ok := p.servers[key]; ok {
		p.mu.Unlock()
		return c, lang, nil
	}
	// место кончилось — гасим самый давний
	if len(p.servers) >= p.max {
		p.evictOldest()
	}
	p.mu.Unlock()

	c, err := Start(ctx, root, lang.Command, initFor(lang, root))
	if err != nil {
		return nil, lang, err
	}
	p.mu.Lock()
	p.servers[key] = c
	p.mu.Unlock()
	log.Printf("языковой сервер %s поднят для %s", lang.Name, root)
	return c, lang, nil
}

// EnsureOpen — открыть документ один раз на сервер.
func (p *Pool) EnsureOpen(c *Client, lang Lang, uri, text string) error {
	key := c.Root() + "\n" + uri
	p.mu.Lock()
	already := p.opened[key]
	p.opened[key] = true
	p.mu.Unlock()
	if already && text == "" {
		return nil
	}
	return c.Open(uri, lang.Name, text)
}

func (p *Pool) evictOldest() {
	var oldKey string
	var oldest time.Time
	for k, c := range p.servers {
		if oldKey == "" || c.LastUse().Before(oldest) {
			oldKey, oldest = k, c.LastUse()
		}
	}
	if oldKey != "" {
		_ = p.servers[oldKey].Close()
		delete(p.servers, oldKey)
		p.forgetOpened(oldKey)
		log.Printf("языковой сервер остановлен: место в пуле")
	}
}

// sweep — остановка по бездействию.
func (p *Pool) sweep() {
	t := time.NewTicker(time.Minute)
	for range t.C {
		p.mu.Lock()
		for k, c := range p.servers {
			if time.Since(c.LastUse()) > p.idle {
				_ = c.Close()
				delete(p.servers, k)
				p.forgetOpened(k)
				log.Printf("языковой сервер остановлен: тишина")
			}
		}
		p.mu.Unlock()
	}
}

// Alive — что сейчас поднято: для экрана.
func (p *Pool) Alive() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for k, c := range p.servers {
		root, lang, _ := strings.Cut(k, "\n")
		out = append(out, fmt.Sprintf("%s · %s · тишина %s", lang, filepath.Base(root),
			time.Since(c.LastUse()).Round(time.Second)))
	}
	return out
}

func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, c := range p.servers {
		_ = c.Close()
		delete(p.servers, k)
	}
}

// forgetOpened забывает открытые документы остановленного сервера: иначе
// после его перезапуска мы решим, что файл уже открыт, и получим пустое
// оглавление вместо ответа.
func (p *Pool) forgetOpened(serverKey string) {
	root, _, _ := strings.Cut(serverKey, "\n")
	for k := range p.opened {
		if strings.HasPrefix(k, root+"\n") {
			delete(p.opened, k)
		}
	}
}

// initFor — подстановка {root} в параметрах запуска сервера.
func initFor(l Lang, root string) map[string]any {
	if len(l.Init) == 0 {
		return nil
	}
	out := map[string]any{}
	for k, v := range l.Init {
		out[k] = substRoot(v, root)
	}
	return out
}

func substRoot(v any, root string) any {
	switch t := v.(type) {
	case string:
		return strings.ReplaceAll(t, "{root}", root)
	case map[string]any:
		out := map[string]any{}
		for k, inner := range t {
			out[k] = substRoot(inner, root)
		}
		return out
	default:
		return v
	}
}
