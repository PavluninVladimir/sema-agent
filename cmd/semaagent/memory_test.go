package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	semalink "github.com/PavluninVladimir/sema-agent/internal/link"
)

// Оборванный провод — это ошибка про связь, а не пустой ответ: пустоту
// агент разработки прочитает как «в памяти ничего нет» и запишет
// неверный вывод. Сервер MCP при этом остаётся жив.
func TestMemoryWithoutLink(t *testing.T) {
	srv := &server{}
	srv.tools = append(srv.tools, memoryTools(nil)...)
	out := call(t, srv, "memory_recall", map[string]any{"path": t.TempDir(), "query": "что угодно"})
	if !strings.Contains(out, "память") && !strings.Contains(out, "Память") {
		t.Fatalf("про память ни слова: %q", out)
	}
	if !strings.Contains(out, "связ") {
		t.Fatalf("про связь ни слова: %q", out)
	}
	// сервер жив: следующий запрос обслуживается как обычно
	if resp := srv.handle(request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"}, &session{}); resp == nil || resp.Error != nil {
		t.Fatalf("сервер MCP не пережил отказ памяти: %+v", resp)
	}
}

// Сёма молчит — говорим об этом, а не выдумываем ответ.
func TestMemoryDidNotAnswer(t *testing.T) {
	srv := &server{}
	srv.tools = append(srv.tools, memoryTools(liar{})...)
	out := call(t, srv, "memory_recall", map[string]any{"path": t.TempDir(), "query": "вопрос"})
	if !strings.Contains(out, "не ответила") {
		t.Fatalf("молчание Сёмы выглядит как ответ: %q", out)
	}
}

// Дверь одна: семь инструментов кода и пять памяти в одном списке.
func TestTwelveTools(t *testing.T) {
	srv := &server{}
	srv.tools = append(srv.tools, memoryTools(nil)...)
	names := map[string]bool{}
	for _, tl := range srv.tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"memory_recall", "memory_remember", "memory_session_start",
		"memory_session_end", "code_knowledge"} {
		if !names[want] {
			t.Fatalf("нет инструмента %s", want)
		}
		if !isMemoryTool(want) {
			t.Fatalf("%s не опознан как память", want)
		}
	}
}

// Имя того, кто спрашивает, доезжает до Сёмы: области выданы паре
// «агент разработки плюс папка», и подмена имени слила бы всех в одного.
func TestAskerNameReachesSema(t *testing.T) {
	c := &echoCaller{}
	srv := &server{}
	srv.tools = append(srv.tools, memoryTools(c)...)
	sess := &session{}
	if resp := srv.handle(request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "initialize",
		Params: json.RawMessage(`{"clientInfo":{"name":"claude-code"}}`)}, sess); resp == nil {
		t.Fatal("initialize без ответа")
	}
	if sess.client != "Claude Code" {
		t.Fatalf("имя клиента не запомнилось: %q", sess.client)
	}
	call(t, srvWith(srv, sess), "memory_recall", map[string]any{"path": t.TempDir(), "query": "вопрос"})
	if c.args["client"] != "Claude Code" {
		t.Fatalf("имя не доехало до Сёмы: %#v", c.args)
	}
	if p, _ := c.args["path"].(string); p == "" {
		t.Fatalf("папка не доехала до Сёмы: %#v", c.args)
	}
}

// ---- подпорки ----

type withSession struct {
	*server
	sess *session
}

func srvWith(s *server, sess *session) *withSession {
	return &withSession{server: s, sess: sess}
}

type liar struct{}

func (liar) Call(context.Context, string, any) (json.RawMessage, error) {
	return nil, errors.New("нет связи с Сёмой")
}

type echoCaller struct{ args map[string]any }

func (e *echoCaller) Call(_ context.Context, _ string, params any) (json.RawMessage, error) {
	raw, _ := json.Marshal(params)
	_ = json.Unmarshal(raw, &e.args)
	return json.RawMessage(`{"text":"ладно"}`), nil
}

// call — один вызов инструмента через тот же разбор, что у клиента MCP.
func call(t *testing.T, s any, name string, args map[string]any) string {
	t.Helper()
	var srv *server
	sess := &session{}
	switch v := s.(type) {
	case *server:
		srv = v
	case *withSession:
		srv, sess = v.server, v.sess
	}
	body, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		t.Fatal(err)
	}
	resp := srv.handle(request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: body}, sess)
	if resp == nil {
		t.Fatal("ответа нет")
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Content) == 0 {
		t.Fatalf("ответ не разобран: %s", bytes.TrimSpace(raw))
	}
	return out.Content[0].Text
}

// Выкат Сёмы посреди работы: провод рвётся, житель машины перезванивает,
// вопрос повторяется. Клиент разницы не видит — он получает ответ, а не
// «памяти нет».
func TestSurvivesSemaRedeploy(t *testing.T) {
	c := &flaky{fails: 2}
	srv := &server{}
	srv.tools = append(srv.tools, memoryTools(c)...)
	out := call(t, srv, "memory_recall", map[string]any{"path": t.TempDir(), "query": "вопрос"})
	if out != "вспомнил" {
		t.Fatalf("обрыв доехал до клиента: %q", out)
	}
	if c.total < 3 {
		t.Fatalf("повторов не было: %d", c.total)
	}
}

// flaky — провод, который первые несколько раз отвечает обрывом.
type flaky struct {
	fails int
	total int
}

func (r *flaky) Call(context.Context, string, any) (json.RawMessage, error) {
	r.total++
	if r.fails > 0 {
		r.fails--
		return nil, semalink.ErrNoLink
	}
	return json.RawMessage(`{"text":"вспомнил"}`), nil
}
