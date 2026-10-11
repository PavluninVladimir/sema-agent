package main

// Хуки памяти внутри бинарника. Раньше это были четыре коротких скрипта
// на каждого агента разработки: они сами искали корень проекта, сами
// разбирали стенограмму и сами ходили к Сёме с токеном в переменной
// окружения. Токен уехал вместе с сетевой дорогой, а определение корня
// и разбор стенограммы жили в двух копиях на shell — теперь они здесь,
// в одном месте и под тестом.
//
// Хук всегда завершается с нулевым кодом: сессия агента разработки не
// должна падать из-за памяти. Молчание при этом не прячется — пропажу
// жителя машины Сёма показывает уведомлением об обрыве связи.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// hookEvent — то, что приходит хуку от Claude Code и от Codex. Поля
// разные, но общий вид один, поэтому разбираем всё сразу.
type hookEvent struct {
	CWD             string `json:"cwd"`
	Source          string `json:"source"`
	Model           string `json:"model"`
	TranscriptPath  string `json:"transcript_path"`
	AgentTranscript string `json:"agent_transcript_path"`
	AgentType       string `json:"agent_type"`
	Prompt          string `json:"prompt"`
	LastAssistant   string `json:"last_assistant_message"`
}

// runHook — один вид события. Возвращает текст, который агент разработки
// положит себе в контекст (пусто — печатать нечего).
func runHook(kind, agent, home string, in io.Reader) string {
	var e hookEvent
	if raw, err := io.ReadAll(io.LimitReader(in, 1<<20)); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &e)
	}
	root := WorkRoot(e.CWD)
	// след хука — до всего остального: Codex не пишет о запуске хуков в
	// свой журнал, и без этой строки нечем отличить «хук не запустили» от
	// «хук не достучался до демона»
	hookTrace(home, fmt.Sprintf("%s от «%s» cwd=%q root=%q prompt=%d last=%d transcript=%q",
		kind, agent, e.CWD, root, len(e.Prompt), len(e.LastAssistant), e.TranscriptPath))
	if root == "" {
		return ""
	}
	switch kind {
	case "session-start":
		model := e.Model
		if model == "" {
			model = strings.ToLower(strings.ReplaceAll(agent, " ", "-"))
		}
		// возобновление и сжатие контекста продолжают ту же сессию, а не
		// открывают новую: иначе одна работа рассыпается на пять сводок
		resume := e.Source != "" && e.Source != "startup"
		text, alive := hookCall(home, "memory_session_start", map[string]any{
			"path": root, "model": model, "resume": resume, "client": agent, hookArg: true})
		if !alive {
			return ""
		}
		return sessionStartText(agent, text)
	case "note", "subagent-note":
		text := noteText(kind, agent, e)
		if text == "" {
			return ""
		}
		hookNote(home, root, agent, text)
		return ""
	case "session-end":
		text, _ := hookCall(home, "memory_session_end", map[string]any{"path": root, "client": agent, hookArg: true})
		return text
	}
	return ""
}

// Подсказка о карточках кода. За месяц карточки спросили пять раз, а
// файлы читали целиком: инструменты жителя у Claude Code отложены и
// модели не видны, пока она их не загрузит. CLAUDE.md читает один клиент
// в одной папке; ответ хука приходит каждому и в любой папке — поэтому
// подсказка живёт здесь. Не длиннее пяти строк, идёт после памяти.
const cardsHint = "Код понимать дешевле карточками жителя машины (sema-agent): " +
	"symbols — оглавление файла, context — карточка символа, impact — где его зовут; " +
	"файл целиком читать, когда карточки мало."

const cardsHintClaude = "У Claude Code эти инструменты отложены: перед первым вызовом загрузите их — " +
	`ToolSearch "select:mcp__sema-agent__symbols,mcp__sema-agent__context,mcp__sema-agent__impact".`

// sessionStartText — что агент разработки получает на старте: память
// (если есть) и подсказка. Памяти нет — подсказка всё равно нужна:
// карточки живут у жителя машины и без Сёмы.
func sessionStartText(agent, memory string) string {
	hint := cardsHint
	if !strings.EqualFold(agent, "Codex") {
		hint += "\n" + cardsHintClaude
	}
	if strings.TrimSpace(memory) == "" {
		return hint
	}
	return strings.TrimRight(memory, "\n") + "\n\n" + hint
}

// noteText — что дописать в сводку сессии. Codex отдаёт реплики прямо в
// хук, Claude Code — только путь к стенограмме.
func noteText(kind, agent string, e hookEvent) string {
	if kind == "subagent-note" {
		last := lastAssistant(e)
		if last == "" {
			return ""
		}
		who := e.AgentType
		if who == "" {
			who = "субагент"
		}
		// работа субагента идёт мимо стенограммы главной сессии: не
		// записать её — значит потерять ревью, аудит и разбор целиком
		return "Субагент (" + who + "): " + clip(last, 1500)
	}
	if e.Prompt != "" {
		return "User: " + clip(e.Prompt, 700)
	}
	if e.LastAssistant != "" {
		return "Agent: " + clip(e.LastAssistant, 700)
	}
	if e.TranscriptPath == "" {
		return ""
	}
	lines := transcriptTail(e.TranscriptPath, 2)
	return strings.Join(lines, "\n")
}

func lastAssistant(e hookEvent) string {
	if e.LastAssistant != "" {
		return e.LastAssistant
	}
	// у субагента бывает своя стенограмма: в общей его работы нет ни
	// строкой, и итог взять больше неоткуда
	if e.AgentTranscript != "" {
		if last := transcriptLast(e.AgentTranscript, "assistant"); last != "" {
			return last
		}
	}
	return transcriptLast(e.TranscriptPath, "assistant")
}

// transcriptRec — строка стенограммы Claude Code: JSON на строку.
type transcriptRec struct {
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// recText — текст реплики. Содержимое бывает строкой и списком кусков;
// нас интересуют только куски вида text, остальное — вызовы инструментов.
func recText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
			out = append(out, p.Text)
		}
	}
	return strings.TrimSpace(strings.Join(out, " "))
}

// transcriptTail — последние n реплик человека и агента подписанными
// строками. Стенограмма бывает в сотни мегабайт, поэтому читаем построчно
// и держим только хвост.
func transcriptTail(path string, n int) []string {
	if path == "" || n <= 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	tail := make([]string, 0, n)
	for _, line := range fileLines(f) {
		var rec transcriptRec
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec.Type != "user" && rec.Type != "assistant" {
			continue
		}
		text := recText(rec.Message.Content)
		if text == "" {
			continue
		}
		who := "Agent: "
		if rec.Type == "user" {
			who = "User: "
		}
		tail = append(tail, who+clip(text, 700))
		if len(tail) > n {
			tail = tail[len(tail)-n:]
		}
	}
	return tail
}

// transcriptLast — последняя реплика заданного вида целиком: итог
// субагента это его последнее слово.
func transcriptLast(path, kind string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	last := ""
	for _, line := range fileLines(f) {
		var rec transcriptRec
		if json.Unmarshal(line, &rec) != nil || rec.Type != kind {
			continue
		}
		if text := recText(rec.Message.Content); text != "" {
			last = text
		}
	}
	return last
}

// fileLines — строки стенограммы по одной. Не Scanner: у него предел
// на длину строки, и строка длиннее предела обрывает разбор молча — а
// стенограмма это одна строка на реплику, и одна большая реплика унесла
// бы весь хвост.
func fileLines(f *os.File) [][]byte {
	r := bufio.NewReaderSize(f, 64<<10)
	var out [][]byte
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			out = append(out, line)
		}
		if err != nil {
			return out
		}
	}
}

// clip — обрезка по рунам, а не по байтам: русский текст режется вдвое
// раньше и доезжает половиной слова.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// hookCall — спросить демона. Не ответил — молчим: у хука нет права
// ронять сессию агента разработки. Второе значение — ответил ли демон
// вообще: «Сёмы нет» и «жителя нет» — разные беды, и подсказка про
// карточки нужна в первой, но бессмысленна во второй.
func hookCall(home, tool string, args map[string]any) (string, bool) {
	c, err := dialLocal(socketPath(home))
	if err != nil {
		return "", false
	}
	defer c.Close()
	resp, err := c.ask(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	if err != nil || resp == nil {
		return "", false
	}
	if resp.Error != nil {
		return "", true
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		return "", true
	}
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if json.Unmarshal(raw, &res) != nil || res.IsError || len(res.Content) == 0 {
		return "", true
	}
	return res.Content[0].Text, true
}

// hookNote — заметка в открытую сессию. Инструментом MCP она не была
// никогда: её шлёт хук, а не модель.
func hookNote(home, path, agent, text string) {
	c, err := dialLocal(socketPath(home))
	if err != nil {
		return
	}
	defer c.Close()
	_, _ = c.ask(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "memory/note",
		"params": map[string]any{"path": path, "text": text, "client": agent}})
}

// codexEvent — как это событие называется у Codex. Печатать всегда
// «SessionStart» значило бы сообщать ему неправду о том, что случилось.
func codexEvent(kind string) string {
	switch kind {
	case "note":
		return "Stop"
	case "subagent-note":
		return "SubagentStop"
	case "session-end":
		return "SessionEnd"
	}
	return "SessionStart"
}

// printHook — как отдать текст агенту разработки. Claude Code кладёт в
// контекст всё, что хук напечатал; Codex ждёт JSON с полем внутри.
func printHook(kind, agent, text string) {
	if strings.EqualFold(agent, "Codex") {
		out := map[string]any{
			"hookSpecificOutput": map[string]any{"hookEventName": codexEvent(kind)},
			"suppressOutput":     true,
		}
		if text != "" {
			out["hookSpecificOutput"].(map[string]any)["additionalContext"] = text
		}
		body, err := json.Marshal(out)
		if err != nil {
			return
		}
		fmt.Println(string(body))
		return
	}
	if text != "" {
		fmt.Println(text)
	}
}

// hookTrace — строка в hooks.log домашней папки агента. Файл маленький и
// свой: по нему видно, что хук вообще запускали и с чем.
func hookTrace(home, line string) {
	if home == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(home, "hooks.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006/01/02 15:04:05"), line)
}
