package link

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// Провод у агента один — последний открытый. Шлюзу Сёма звонит заново,
// когда своё соединение счёл мёртвым, а шлюз этого ещё не заметил; 5
// октября 2026 старое соединение, закрываясь, стёрло новое, и шлюз отвечал
// 502 при живом проводе (agent-gateway, 8.1–8.3).

// semaSide — Сёма на том конце трубы: читает кадры и отвечает на вопросы
// с номером, пока не велено молчать.
type semaSide struct {
	conn   net.Conn
	silent atomic.Bool
	pings  atomic.Int32
	asked  atomic.Int32 // вопросов, кроме пинга
}

func (s *semaSide) serve() {
	r := bufio.NewReader(s.conn)
	for {
		body, err := readFramed(r)
		if err != nil {
			return
		}
		var m struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(body, &m) != nil || m.ID == nil || m.Method == "" {
			continue
		}
		if m.Method == "ping" {
			s.pings.Add(1)
		} else {
			s.asked.Add(1)
		}
		if s.silent.Load() {
			continue
		}
		out := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":"ответ на %s"}`, *m.ID, m.Method)
		_, _ = fmt.Fprintf(s.conn, "Content-Length: %d\r\n\r\n%s", len(out), out)
	}
}

// call — Сёма звонит шлюзу: соединение уходит в Hold, его конец — нам.
func call(t *testing.T, ctx context.Context, c *Conn) (*semaSide, <-chan error) {
	t.Helper()
	ours, theirs := net.Pipe()
	s := &semaSide{conn: theirs}
	t.Cleanup(func() { _ = theirs.Close() })
	held := make(chan error, 1)
	go func() { held <- c.Hold(ctx, ours) }()
	go s.serve()
	return s, held
}

func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("не дождались: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func closed(t *testing.T, held <-chan error, what string) {
	t.Helper()
	select {
	case <-held:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s не закрылось", what)
	}
}

// Новое соединение закрывает прежнее, а прежнее, уходя, новое не трогает:
// провод жив, и вопрос Сёме идёт по новому.
func TestNewLinkSurvivesOldOneClosing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(Identity{Name: "шлюз"}, "", nil, "шлюз", "тест", nil)

	_, heldA := call(t, ctx, c)
	until(t, "первое соединение", c.Live)
	_, heldB := call(t, ctx, c)
	closed(t, heldA, "прежнее соединение") // 8.2: его закрыл приход нового
	if !c.Live() {
		t.Fatal("прежнее соединение, закрываясь, стёрло новое — шлюз отвечал бы 502 при живом проводе")
	}
	cctx, ccancel := context.WithTimeout(ctx, 2*time.Second)
	defer ccancel()
	raw, err := c.Call(cctx, "шлюз.поток", map[string]any{"поток": "x"})
	if err != nil || string(raw) != `"ответ на шлюз.поток"` {
		t.Fatalf("вопрос по новому соединению: %s, %v", raw, err)
	}

	// закрылось текущее — связи нет
	c.Reconnect()
	closed(t, heldB, "текущее соединение")
	if c.Live() {
		t.Fatal("текущее соединение закрылось, а провод считается живым")
	}
}

// Вопрос, ушедший прежним соединением, узнаёт об его закрытии сразу, а не
// по своему сроку: ответ по новому соединению на него не придёт.
func TestQuestionOnReplacedLinkFailsAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(Identity{Name: "шлюз"}, "", nil, "шлюз", "тест", nil)

	a, heldA := call(t, ctx, c)
	a.silent.Store(true)
	until(t, "первое соединение", c.Live)
	asked := make(chan error, 1)
	go func() {
		cctx, ccancel := context.WithTimeout(ctx, 10*time.Second)
		defer ccancel()
		_, err := c.Call(cctx, "шлюз.поток", nil)
		asked <- err
	}()
	until(t, "вопрос ушёл прежним соединением", func() bool { return a.asked.Load() == 1 })
	call(t, ctx, c)
	closed(t, heldA, "прежнее соединение")
	select {
	case err := <-asked:
		if !errors.Is(err, ErrNoLink) {
			t.Fatalf("вопрос по закрытому соединению вернул %v, ждали «нет связи»", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("вопрос по закрытому соединению висит до своего срока")
	}
	if !c.Live() {
		t.Fatal("новое соединение пропало")
	}
}

// На пинг молчат — соединение закрывается, не дожидаясь таймаута TCP;
// отвечают — живёт (8.4).
func TestSilentLinkClosedByPing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(Identity{Name: "шлюз"}, "", nil, "шлюз", "тест", nil)
	c.PingEvery, c.PingWait = 30*time.Millisecond, 100*time.Millisecond

	s, held := call(t, ctx, c)
	until(t, "соединение", c.Live)
	until(t, "три пинга с ответом", func() bool { return s.pings.Load() >= 3 })
	if !c.Live() {
		t.Fatal("соединение, отвечающее на пинг, закрылось")
	}
	select {
	case <-held:
		t.Fatal("соединение, отвечающее на пинг, закрылось")
	default:
	}

	s.silent.Store(true)
	closed(t, held, "молчащее соединение")
	if c.Live() {
		t.Fatal("молчащее соединение закрыто, а провод считается живым")
	}
}
