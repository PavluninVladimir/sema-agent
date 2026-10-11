package main

// Тонкая обёртка: Claude Code и Codex запускают агента по stdio каждый
// свой, но языковой сервер должен подниматься один раз на всех. Поэтому
// обёртка не держит пул, а пересылает разговор запущенному демону через
// местную дверь — сокет в домашней папке агента.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"
)

// withWorkRoot — подставить рабочую папку в вызов памяти. Обёртку
// запускает агент разработки из папки проекта, а демон живёт в своей
// папке и о проекте не знает: спросить папку больше некого.
//
// Инструментам кода папку не подставляем: у них path — это путь к файлу,
// и чужое значение там сломало бы ответ.
func withWorkRoot(line []byte) []byte {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  struct {
			Name string         `json:"name"`
			Args map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &req) != nil || req.Method != "tools/call" || !isMemoryTool(req.Params.Name) {
		return line
	}
	if p, ok := req.Params.Args["path"].(string); ok && strings.TrimSpace(p) != "" {
		return line
	}
	wd, err := os.Getwd()
	if err != nil {
		return line
	}
	if req.Params.Args == nil {
		req.Params.Args = map[string]any{}
	}
	req.Params.Args["path"] = WorkRoot(wd)
	out, err := json.Marshal(req)
	if err != nil {
		return line
	}
	return out
}

// proxyLocal — обёртка через местную дверь: одно соединение на всю
// сессию агента разработки. Соединение постоянное не ради скорости, а
// ради памяти: демон запоминает по нему, кто спрашивает, и области
// выдаются тому, кто пришёл, а не жителю машины.
func proxyLocal(sock string) {
	c, err := dialLocal(sock)
	if err != nil {
		log.Printf("демон не отвечает: %v", err)
		return
	}
	defer func() { c.Close() }()
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	enc := json.NewEncoder(os.Stdout)
	// знакомство запоминаем: демон помнит по соединению, кто спрашивает,
	// и после переподключения ему надо представиться заново
	var handshake []byte
	for {
		line, err := in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			line = withWorkRoot(line)
			var req request
			_ = json.Unmarshal(line, &req)
			if req.Method == "initialize" {
				handshake = append([]byte(nil), bytes.TrimSpace(line)...)
			}
			resp, aerr := exchange(c, line, len(req.ID) > 0)
			if aerr != nil {
				// демона перезапустили (выкат, пересборка) — сессия
				// агента разработки от этого не должна остаться без
				// памяти и карточек кода до самого конца
				if c2, rerr := reconnect(sock, handshake); rerr == nil {
					c.Close()
					c = c2
					resp, aerr = exchange(c, line, len(req.ID) > 0)
				}
			}
			switch {
			case aerr != nil:
				if len(req.ID) > 0 {
					_ = enc.Encode(response{JSONRPC: "2.0", ID: req.ID,
						Error: &rpcError{Code: -32000, Message: "демон агента не отвечает: " + aerr.Error()}})
				}
				log.Printf("демон не ответил: %v", aerr)
			case len(bytes.TrimSpace(resp)) > 0:
				_, _ = os.Stdout.Write(append(bytes.TrimSpace(resp), '\n'))
			}
		}
		if err != nil {
			return
		}
	}
}

// exchange — кадр туда, ответ обратно. Уведомление ответа не ждёт: демон на
// него молчит, и ожидание повисло бы навсегда.
func exchange(c *localClient, line []byte, wantReply bool) ([]byte, error) {
	if err := c.sendRaw(line); err != nil {
		return nil, err
	}
	if !wantReply {
		return nil, nil
	}
	return c.readRaw()
}

// reconnect — новая дверь и повторное знакомство. Несколько
// попыток: демон под launchd поднимается меньше чем за секунду, но
// пересборка занимает дольше.
func reconnect(sock string, handshake []byte) (*localClient, error) {
	var last error
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		c, err := dialLocal(sock)
		if err != nil {
			last = err
			continue
		}
		if len(handshake) > 0 {
			if _, err := exchange(c, handshake, true); err != nil {
				c.Close()
				last = err
				continue
			}
		}
		log.Printf("демон вернулся, продолжаем")
		return c, nil
	}
	return nil, last
}
