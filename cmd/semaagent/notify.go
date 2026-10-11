package main

// Уведомления Сёмы на этой машине: он зовёт по проводу, житель машины
// показывает и отвечает, показал ли. Пропущенное за время сна ноутбука
// он добирает сам при подключении — push по проводу этого не покрывает,
// а терять записи нельзя.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/PavluninVladimir/sema-agent/internal/desk"
	semalink "github.com/PavluninVladimir/sema-agent/internal/link"
)

// notifier — показ уведомлений и значок у часов.
type notifier struct {
	desk *desk.Desk
	conn *semalink.Conn
	home string

	mu   sync.Mutex
	last int64 // номер последней показанной записи
}

// deskState — что житель машины помнит между запусками. Сёма помнит
// доставку, но не то, что человек увидел на этой машине.
type deskState struct {
	Last int64 `json:"последняя"`
}

func statePath(home string) string { return filepath.Join(home, "state.json") }

func newNotifier(home string, conn *semalink.Conn) *notifier {
	n := &notifier{conn: conn, home: home}
	if raw, err := os.ReadFile(statePath(home)); err == nil {
		var st deskState
		if json.Unmarshal(raw, &st) == nil {
			n.last = st.Last
		}
	}
	n.desk = desk.New(home, n.open, n.reconnect)
	return n
}

// Start — поднять рабочий стол: значок у часов и помощника.
func (n *notifier) Start() { n.desk.Start() }

// save — номер последней показанной записи. Двигается только после
// показа: перезапуск не должен показывать старое заново.
func (n *notifier) save(id int64) {
	n.mu.Lock()
	if id > n.last {
		n.last = id
	}
	last := n.last
	n.mu.Unlock()
	raw, err := json.Marshal(deskState{Last: last})
	if err != nil {
		return
	}
	_ = os.WriteFile(statePath(n.home), raw, 0o600)
}

// show — показать запись. Ответ честный: Сёма пометит доставленной
// только то, что человек мог увидеть.
func (n *notifier) show(c desk.Card) (any, error) {
	if err := n.desk.Show(c); err != nil {
		return map[string]any{"показал": false, "почему": err.Error()}, nil
	}
	n.save(c.ID)
	return map[string]any{"показал": true}, nil
}

// handlers — способность «уведомления» с этой стороны провода.
func (n *notifier) handlers() map[string]semalink.Handler {
	return map[string]semalink.Handler{
		"уведомления.показать": func(ctx context.Context, params json.RawMessage) (any, error) {
			var c desk.Card
			if err := json.Unmarshal(params, &c); err != nil {
				return nil, fmt.Errorf("уведомление не разобрано: %w", err)
			}
			return n.show(c)
		},
		"уведомления.счётчик": func(ctx context.Context, params json.RawMessage) (any, error) {
			var p struct {
				Wait int    `json:"ждут"`
				Addr string `json:"адрес"`
			}
			_ = json.Unmarshal(params, &p)
			n.desk.Count(p.Wait)
			if p.Addr != "" {
				n.desk.Address(p.Addr)
			}
			return map[string]any{"принял": true}, nil
		},
	}
}

// catchUp — добрать пропущенное: связь появилась, а записи за время сна
// ноутбука ждут у Сёмы.
func (n *notifier) catchUp() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	n.mu.Lock()
	after := n.last
	n.mu.Unlock()
	raw, err := n.conn.Call(ctx, "уведомления.лента", map[string]any{"после": after, "сколько": 20})
	if err != nil {
		log.Printf("уведомления: пропущенное не спросили: %v", err)
		return
	}
	var res struct {
		Cards []desk.Card `json:"записи"`
		Wait  int         `json:"ждут"`
		Addr  string      `json:"адрес"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		log.Printf("уведомления: лента не разобрана: %v", err)
		return
	}
	if res.Addr != "" {
		n.desk.Address(res.Addr)
	}
	n.desk.Count(res.Wait)
	for _, c := range res.Cards {
		if _, err := n.show(c); err != nil {
			log.Printf("уведомления: %v", err)
		}
	}
	if len(res.Cards) > 0 {
		log.Printf("уведомления: показал пропущенное: %d", len(res.Cards))
	}
}

// open — нажали на уведомление или на пункт меню: открываем нужное в
// браузере. Житель машины сам ничего не выполняет — только показывает.
func (n *notifier) open(screen string) {
	if screen == "$журнал" {
		_ = exec.Command("/usr/bin/open", filepath.Join(n.home, "agent.log")).Run()
		return
	}
	addr := strings.TrimRight(n.desk.Addr(), "/")
	if addr == "" {
		log.Printf("уведомления: адрес Сёмы неизвестен, открывать нечего")
		return
	}
	if screen == "" {
		screen = "notifications"
	}
	_ = exec.Command("/usr/bin/open", addr+"/#screen="+screen).Run()
}

// reconnect — «Переподключиться» из меню: рвём соединение, провод
// перезвонит сам.
func (n *notifier) reconnect() {
	log.Printf("связь с Сёмой: переподключаюсь по просьбе человека")
	n.conn.Reconnect()
}

// linkState — связь появилась или пропала: значок у часов говорит ровно
// об этом, а появление связи повод добрать пропущенное.
func (n *notifier) linkState(up bool, why string) {
	n.desk.Link(up, time.Now(), why)
	if up {
		go n.catchUp()
	}
}
