package main

// Сервер MCP агента: тот же JSON-RPC, что у Сёмы, но живёт на машине с
// файлами. Говорит двумя ртами сразу: stdio — для агента разработки,
// запустившего его процессом, и сокет в домашней папке — для обёрток и
// хуков. Пул языковых серверов у них общий.
//
// Сетевого рта нет: он остался с тех пор, когда Сёма ходил к агенту по
// HTTP с токеном. Сейчас он ходит по своему проводу, а второй путь
// только расширял бы поверхность — и требовал секрета в настройках.

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/PavluninVladimir/sema-agent/internal/lsp"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// tool — инструмент агента.
type tool struct {
	Name   string
	Desc   string
	Schema map[string]any
	Run    func(args map[string]any) (string, error)
}

type server struct {
	tools []tool
	pool  *lsp.Pool
	sema  memoryCaller // провод к Сёме; nil — работаем в одиночку
	mu    sync.Mutex
	saved savings
	store string // файл со счётом, чтобы перезапуск не обнулял пользу
	// knowledgeWait — сколько карточка ждёт приложение из памяти Сёмы;
	// ноль — умолчание knowledgeWaitDefault
	knowledgeWait time.Duration
	principles    principlesKeeper
}

// savings — счёт непрочитанного: без цифр польза не отличается от веры.
// Итог нарастающий, а рядом — строка на день: по одному числу «за всё
// время» не видно, стали ли карточки спрашивать после подсказки в хуке.
type savings struct {
	Calls int `json:"calls"`
	Lines int `json:"saved_lines"`
	Cards int `json:"card_lines"`
	// WithKnowledge — карточек с приложением из памяти Сёмы: по этой доле
	// видно, доходит ли знание до агентов, а не только оглавления
	WithKnowledge int                   `json:"with_knowledge"`
	Days          map[string]daySavings `json:"days,omitempty"`
}

type daySavings struct {
	Calls         int `json:"calls"`
	Lines         int `json:"saved_lines"`
	WithKnowledge int `json:"with_knowledge"`
}

// savingsKeepDays — сколько дней держать разбивку; неделя замера
// укладывается с запасом.
const savingsKeepDays = 30

func (s *server) note(cardLines, savedLines int, knowledge bool) {
	s.mu.Lock()
	s.saved.Calls++
	s.saved.Cards += cardLines
	s.saved.Lines += savedLines
	if knowledge {
		s.saved.WithKnowledge++
	}
	if s.saved.Days == nil {
		s.saved.Days = map[string]daySavings{}
	}
	day := time.Now().Format("2006-01-02")
	d := s.saved.Days[day]
	d.Calls++
	d.Lines += savedLines
	if knowledge {
		d.WithKnowledge++
	}
	s.saved.Days[day] = d
	cut := time.Now().AddDate(0, 0, -savingsKeepDays).Format("2006-01-02")
	for k := range s.saved.Days {
		if k < cut {
			delete(s.saved.Days, k)
		}
	}
	snapshot, path := s.saved, s.store
	s.mu.Unlock()
	if path == "" {
		return
	}
	// счёт живёт между запусками: перезапуск агента — не повод обнулять
	// пользу, которую он уже принёс
	if raw, err := json.Marshal(snapshot); err == nil {
		_ = os.WriteFile(path, raw, 0o600)
	}
}

// loadSavings — поднять счёт с прошлого запуска. Нет файла или он битый —
// начинаем с нуля, это не ошибка.
func (s *server) loadSavings(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = path
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var got savings
	if json.Unmarshal(raw, &got) == nil {
		s.saved = got
	}
}

func (s *server) savedNow() savings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved
}

// savedWeek — сумма за последние семь дней по дневным строкам.
func (s savings) savedWeek() daySavings {
	cut := time.Now().AddDate(0, 0, -7).Format("2006-01-02")
	var w daySavings
	for k, d := range s.Days {
		if k >= cut {
			w.Calls += d.Calls
			w.Lines += d.Lines
			w.WithKnowledge += d.WithKnowledge
		}
	}
	return w
}

func (s *server) byName(name string) *tool {
	for i := range s.tools {
		if s.tools[i].Name == name {
			return &s.tools[i]
		}
	}
	return nil
}

// session — состояние одного разговора: кто на том конце. Для памяти это
// важно: потребитель ищется по паре «кто спрашивает плюс папка», и Claude
// Code с Codex в одной папке — разные потребители с разными сессиями.
type session struct{ client string }

func (s *server) handle(req request, sess *session) *response {
	ok := func(v any) *response { return &response{JSONRPC: "2.0", ID: req.ID, Result: v} }
	fail := func(code int, msg string) *response {
		return &response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: code, Message: msg}}
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ClientInfo struct {
				Name string `json:"name"`
			} `json:"clientInfo"`
		}
		if json.Unmarshal(req.Params, &p) == nil && sess != nil {
			sess.client = clientName(p.ClientInfo.Name)
		}
		return ok(map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "Сёма-агент", "version": "1"},
		})
	case "notifications/initialized", "initialized":
		return nil
	case "resources/list", "resources/templates/list", "prompts/list":
		// ресурсов и подсказок у агента нет; Codex спрашивает их при каждом
		// старте, и отказ «неизвестный метод» ложится ему в журнал ошибкой
		return ok(map[string]any{"resources": []any{}, "resourceTemplates": []any{}, "prompts": []any{}})
	case "tools/list":
		var list []map[string]any
		for _, t := range s.tools {
			list = append(list, map[string]any{
				"name": t.Name, "description": t.Desc, "inputSchema": t.Schema,
			})
		}
		return ok(map[string]any{"tools": list})
	case "memory/note":
		// заметка в открытую сессию: её шлёт хук после каждого ответа,
		// инструментом MCP она не была никогда
		var p struct {
			Path   string `json:"path"`
			Text   string `json:"text"`
			Client string `json:"client"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fail(-32602, "аргументы не разобраны")
		}
		if p.Client == "" && sess != nil {
			p.Client = sess.client
		}
		// хуки не видны ни в журнале Сёмы, ни агенту разработки: единственное
		// место, где видно, что хук вообще пришёл, — журнал демона
		if err := memoryNote(s.sema, p.Path, p.Client, p.Text, hookWait); err != nil {
			log.Printf("заметка от «%s» в %s не дошла: %v", p.Client, p.Path, err)
			return fail(-32000, err.Error())
		}
		log.Printf("заметка от «%s» в %s: %d знаков", p.Client, WorkRoot(p.Path), len([]rune(p.Text)))
		return ok(map[string]any{"ok": true})
	case "tools/call":
		var p struct {
			Name string         `json:"name"`
			Args map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fail(-32602, "аргументы не разобраны")
		}
		t := s.byName(p.Name)
		if t == nil {
			return fail(-32601, "нет такого инструмента: "+p.Name)
		}
		// память спрашивают от имени того, кто на том конце: области
		// выданы паре «агент разработки плюс папка», и подменять его
		// именем жителя машины значило бы слить всех в одного. Карточкам
		// имя нужно по той же причине: приложение к ним берётся из памяти
		if sess != nil && sess.client != "" {
			if p.Args == nil {
				p.Args = map[string]any{}
			}
			if _, ok := p.Args["client"]; !ok {
				p.Args["client"] = sess.client
			}
		}
		text, err := t.Run(p.Args)
		// старт сессии — повод сверить принципы из CLAUDE.md: файл мог
		// измениться с прошлого раза. В фоне, хук ждать не должен
		if p.Name == "memory_session_start" && err == nil {
			s.principlesMaybe(WorkRoot(str(p.Args, "path")), str(p.Args, "client"))
		}
		// обращения к памяти видны в журнале демона: кто, за чем, из какой
		// папки и чем кончилось — иначе «Codex пользуется памятью» нечем
		// ни подтвердить, ни опровергнуть
		if isMemoryTool(p.Name) {
			who, _ := p.Args["client"].(string)
			if err != nil {
				log.Printf("память: %s от «%s» в %v — отказ: %v", p.Name, who, p.Args["path"], err)
			} else {
				log.Printf("память: %s от «%s» в %v — %d знаков", p.Name, who, p.Args["path"], len([]rune(text)))
			}
		}
		if err != nil {
			return ok(map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true,
			})
		}
		return ok(map[string]any{"content": []map[string]any{{"type": "text", "text": text}}})
	}
	return fail(-32601, "неизвестный метод "+req.Method)
}

// serveStdio — рот для Claude Code и Codex: JSON-RPC построчно.
func (s *server) serveStdio(in io.Reader, out io.Writer) {
	r := bufio.NewReaderSize(in, 1<<20)
	enc := json.NewEncoder(out)
	sess := &session{}
	for {
		line, err := r.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var req request
			if json.Unmarshal(line, &req) == nil {
				if resp := s.handle(req, sess); resp != nil {
					_ = enc.Encode(resp)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func str(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func num(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}

func schema(props map[string]string, required ...string) map[string]any {
	p := map[string]any{}
	for name, desc := range props {
		p[name] = map[string]any{"type": "string", "description": desc}
	}
	return map[string]any{"type": "object", "properties": p, "required": required}
}

// savingsPath — где лежит счёт сэкономленного.
func savingsPath(home string) string {
	if home == "" {
		return ""
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return ""
	}
	return filepath.Join(home, "savings.json")
}

// clientName — человеческое имя агента разработки. Совпадать с прежними
// именами обязательно: области памяти выданы «Claude Code» и «Codex», и
// новое написание отрезало бы их от собственных записей.
func clientName(name string) string {
	low := strings.ToLower(strings.TrimSpace(name))
	switch {
	case low == "":
		return ""
	case strings.Contains(low, "claude"):
		return "Claude Code"
	case strings.Contains(low, "codex"):
		return "Codex"
	case strings.Contains(low, "cursor"):
		return "Cursor"
	}
	return strings.TrimSpace(name)
}
