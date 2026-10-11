package lsp

// Клиент языкового сервера: JSON-RPC по stdio с заголовком Content-Length.
// Ровно тот же протокол, что у MCP, только транспорт другой — поэтому
// разбор здесь короткий и без сторонних библиотек.
//
// Языковой сервер знает про код больше любого нашего парсера: типы,
// ссылки, иерархию вызовов, диагностику. Задача клиента — спросить и
// перевести ответ в понятные структуры.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client — один запущенный языковой сервер.
type Client struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	out     *bufio.Reader
	mu      sync.Mutex // одна запись за раз
	waitMu  sync.Mutex
	waiting map[int]chan json.RawMessage
	diagsMu sync.Mutex
	diags   map[string][]Diagnostic // по адресу документа
	id      int
	root    string
	closed  bool
	lastUse time.Time
	init    map[string]any
}

// Start поднимает сервер и здоровается с ним. init — что положить в
// initializationOptions: Volar без пути к TypeScript отвечает молчанием,
// и это не единственный такой сервер.
func Start(ctx context.Context, root string, command []string, init map[string]any) (*Client, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("не задана команда языкового сервера")
	}
	path, err := exec.LookPath(command[0])
	if err != nil {
		return nil, fmt.Errorf("языковой сервер %q не установлен", command[0])
	}
	cmd := exec.Command(path, command[1:]...)
	cmd.Dir = root
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 1<<20),
		waiting: map[int]chan json.RawMessage{}, diags: map[string][]Diagnostic{},
		root: root, lastUse: time.Now()}
	go c.read()
	c.init = init
	if err := c.initialize(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) Root() string       { return c.root }
func (c *Client) LastUse() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.lastUse }

// read — единственный читатель ответа: раздаёт по номерам запросов, а
// уведомления о диагностике складывает по документам.
func (c *Client) read() {
	for {
		body, err := readMessage(c.out)
		if err != nil {
			c.failAll(err)
			return
		}
		var head struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Params json.RawMessage `json:"params"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &head) != nil {
			continue
		}
		switch {
		case head.ID != nil && head.Method == "":
			c.waitMu.Lock()
			ch := c.waiting[*head.ID]
			delete(c.waiting, *head.ID)
			c.waitMu.Unlock()
			if ch == nil {
				continue
			}
			if head.Error != nil {
				ch <- json.RawMessage(`{"__error":` + strconv.Quote(head.Error.Message) + `}`)
				continue
			}
			ch <- head.Result
		case head.Method == "textDocument/publishDiagnostics":
			var p struct {
				URI         string       `json:"uri"`
				Diagnostics []Diagnostic `json:"diagnostics"`
			}
			if json.Unmarshal(head.Params, &p) == nil {
				c.diagsMu.Lock()
				c.diags[p.URI] = p.Diagnostics
				c.diagsMu.Unlock()
			}
		case head.ID != nil:
			// сервер спрашивает нас: отвечаем пустотой, чтобы он не ждал
			_ = c.reply(*head.ID)
		}
	}
}

func (c *Client) failAll(err error) {
	c.waitMu.Lock()
	for id, ch := range c.waiting {
		ch <- json.RawMessage(`{"__error":` + strconv.Quote("языковой сервер отключился: "+err.Error()) + `}`)
		delete(c.waiting, id)
	}
	c.waitMu.Unlock()
}

func readMessage(r *bufio.Reader) ([]byte, error) {
	length := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			length, _ = strconv.Atoi(strings.TrimSpace(value))
		}
	}
	if length <= 0 {
		return nil, fmt.Errorf("заголовок без длины")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func (c *Client) send(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("языковой сервер остановлен")
	}
	c.lastUse = time.Now()
	_, err = fmt.Fprintf(c.in, "Content-Length: %d\r\n\r\n%s", len(raw), raw)
	return err
}

func (c *Client) reply(id int) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": nil})
}

// Notify — уведомление без ответа.
func (c *Client) Notify(method string, params any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// Call — запрос с ответом.
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	c.waitMu.Lock()
	c.id++
	id := c.id
	ch := make(chan json.RawMessage, 1)
	c.waiting[id] = ch
	c.waitMu.Unlock()

	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		c.waitMu.Lock()
		delete(c.waiting, id)
		c.waitMu.Unlock()
		return ctx.Err()
	case raw := <-ch:
		var e struct {
			Err string `json:"__error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Err != "" {
			return fmt.Errorf("%s", e.Err)
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(raw, out)
	}
}

func (c *Client) initialize(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	params := map[string]any{
		"processId": nil,
		"rootUri":   PathToURI(c.root),
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"documentSymbol":     map[string]any{"hierarchicalDocumentSymbolSupport": true},
				"references":         map[string]any{},
				"definition":         map[string]any{},
				"publishDiagnostics": map[string]any{},
				"callHierarchy":      map[string]any{},
			},
			"workspace": map[string]any{"symbol": map[string]any{}},
		},
	}
	if len(c.init) > 0 {
		params["initializationOptions"] = c.init
	}
	if err := c.Call(cctx, "initialize", params, nil); err != nil {
		return err
	}
	return c.Notify("initialized", map[string]any{})
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	_ = c.Notify("shutdown", nil)
	_ = c.in.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_, _ = c.cmd.Process.Wait()
	return nil
}
