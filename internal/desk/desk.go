// Package desk — житель машины на рабочем столе: значок у часов и
// уведомления Сёмы.
//
// Значок и настоящее уведомление — это Cocoa, а агент написан на Go без
// cgo. Поэтому рисует помощник на Swift: маленький бандл с
// LSUIElement (нет значка в Dock), который агент собирает на месте и
// держит своим потомком. Общение — строки JSON по трубе: вниз состояние
// и уведомления, вверх нажатия и ответ «показал».
//
// Помощника собрать нечем (нет swiftc) — уведомления всё равно
// показываются, через osascript, но значка нет, и об этом сказано в
// журнале. Молча делать вид, что значок есть, нельзя.
package desk

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Card — уведомление Сёмы в том виде, в каком его показывают человеку.
type Card struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Text   string `json:"text"`
	Level  string `json:"level"`
	Screen string `json:"screen"`
}

// Desk — рабочий стол машины: значок, меню и уведомления.
type Desk struct {
	home string
	// что делать по нажатию: открыть экран Сёмы, перезвонить, выйти
	open      func(screen string)
	reconnect func()

	mu      sync.Mutex
	cmd     *exec.Cmd
	in      *bufio.Writer
	state   state
	address string
	waiting map[int64]chan shown // ждут ответа помощника «показал»
	helper  bool                 // помощник запущен и отвечает
	told    bool                 // про отсутствие помощника уже сказали
}

type state struct {
	Kind  string `json:"вид"`
	Link  bool   `json:"связь"`
	Since string `json:"с,omitempty"`
	Err   string `json:"ошибка,omitempty"`
	Wait  int    `json:"ждут"`
	Addr  string `json:"адрес,omitempty"`
}

type shown struct {
	OK  bool
	Why string
}

// New — рабочий стол. Сборка помощника идёт в стороне: пока её нет,
// уведомления показываются запасным путём.
func New(home string, open func(screen string), reconnect func()) *Desk {
	return &Desk{home: home, open: open, reconnect: reconnect,
		waiting: map[int64]chan shown{}, state: state{Kind: "состояние"}}
}

// Start — поднять помощника. Отдельно от New: сборка идёт на диске, и
// заводить её должен тот, кто действительно живёт на машине, а не всякий,
// кому понадобился рабочий стол.
func (d *Desk) Start() {
	if runtime.GOOS != "darwin" {
		return
	}
	go d.keepHelper()
}

// Available — умеет ли эта машина показывать уведомления вообще.
// На маке умеет всегда: нет помощника — есть osascript.
func Available() bool { return runtime.GOOS == "darwin" }

// Show — показать уведомление. Возвращает ошибку, если показать не
// удалось: Сёма пометит запись доставленной только по «показал».
func (d *Desk) Show(c Card) error {
	if !Available() {
		return fmt.Errorf("на этой машине показывать нечем (%s)", runtime.GOOS)
	}
	if err := d.showByHelper(c); err == nil {
		return nil
	} else {
		log.Printf("значок: помощник не показал (%v), показываю системой", err)
	}
	return showByScript(c)
}

// showByHelper — показать через помощника и дождаться его ответа.
func (d *Desk) showByHelper(c Card) error {
	d.mu.Lock()
	if !d.helper || d.in == nil {
		d.mu.Unlock()
		return fmt.Errorf("помощник не запущен")
	}
	ch := make(chan shown, 1)
	d.waiting[c.ID] = ch
	err := d.write(map[string]any{"вид": "уведомление", "id": c.ID, "заголовок": c.Title,
		"текст": c.Text, "уровень": c.Level, "экран": c.Screen})
	d.mu.Unlock()
	if err != nil {
		d.forget(c.ID)
		return err
	}
	select {
	case res := <-ch:
		if !res.OK {
			return fmt.Errorf("%s", res.Why)
		}
		return nil
	case <-time.After(10 * time.Second):
		d.forget(c.ID)
		return fmt.Errorf("помощник молчит")
	}
}

func (d *Desk) forget(id int64) {
	d.mu.Lock()
	delete(d.waiting, id)
	d.mu.Unlock()
}

// showByScript — запасной путь: системное уведомление без кнопок. Хуже
// помощника, но человек хотя бы видит, что его ждут.
func showByScript(c Card) error {
	if os.Getenv("SEMA_DESK_QUIET") != "" {
		// в проверках на живой машине всплывающие окна ни к чему
		return fmt.Errorf("тихий режим: показывать нечем")
	}
	esc := func(s string) string {
		s = strings.ReplaceAll(s, "\\", "\\\\")
		return strings.ReplaceAll(s, `"`, `\"`)
	}
	script := fmt.Sprintf(`display notification "%s" with title "Сёма" subtitle "%s"`,
		esc(clip(c.Text, 200)), esc(clip(c.Title, 100)))
	return exec.Command("/usr/bin/osascript", "-e", script).Run()
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Link — состояние провода к Сёме: значок и меню говорят одно и то же.
func (d *Desk) Link(up bool, since time.Time, why string) {
	d.mu.Lock()
	d.state.Link = up
	d.state.Err = why
	if !since.IsZero() {
		d.state.Since = since.Format("15:04")
	}
	d.mu.Unlock()
	d.push()
}

// Count — сколько записей ждёт человека: точка у значка и число в меню.
func (d *Desk) Count(n int) {
	d.mu.Lock()
	d.state.Wait = n
	d.mu.Unlock()
	d.push()
}

// Address — адрес Сёмы в браузере: по нему открываются экраны.
func (d *Desk) Address(addr string) {
	d.mu.Lock()
	d.state.Addr = addr
	d.address = addr
	d.mu.Unlock()
	d.push()
}

// Addr — известный адрес Сёмы.
func (d *Desk) Addr() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.address
}

func (d *Desk) push() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.helper || d.in == nil {
		return
	}
	_ = d.write(d.state)
}

// write — строка помощнику. Зовётся под замком.
func (d *Desk) write(v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := d.in.Write(append(body, '\n')); err != nil {
		return err
	}
	return d.in.Flush()
}

// keepHelper — помощник должен жить: упал — поднимаем с растущей паузой,
// как провод. Уведомления при этом не теряются: непоказанное уходит
// запасным путём, а Сёма узнаёт правду в ответе.
func (d *Desk) keepHelper() {
	wait := time.Second
	for {
		started := time.Now()
		if err := d.runHelper(); err != nil {
			d.sayOnce(err)
		}
		if time.Since(started) > time.Minute {
			wait = time.Second
		}
		time.Sleep(wait)
		if wait < time.Minute {
			wait *= 2
		}
	}
}

// sayOnce — про отсутствие помощника говорим один раз: журнал не должен
// заполняться одной и той же строкой каждую минуту.
func (d *Desk) sayOnce(err error) {
	d.mu.Lock()
	first := !d.told
	d.told = true
	d.mu.Unlock()
	if first {
		log.Printf("значок у часов: %v — уведомления показываю системой, значка не будет", err)
	}
}

func (d *Desk) runHelper() error {
	path, err := buildHelper(d.home)
	if err != nil {
		return err
	}
	cmd := exec.Command(path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	d.mu.Lock()
	d.cmd, d.in, d.helper, d.told = cmd, bufio.NewWriter(stdin), true, false
	st := d.state
	d.mu.Unlock()
	log.Printf("значок у часов: помощник запущен (%s)", path)
	d.mu.Lock()
	_ = d.write(st)
	d.mu.Unlock()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		d.fromHelper(sc.Bytes())
	}
	err = cmd.Wait()
	d.mu.Lock()
	d.helper, d.in, d.cmd = false, nil, nil
	for id, ch := range d.waiting {
		ch <- shown{OK: false, Why: "помощник закрылся"}
		delete(d.waiting, id)
	}
	d.mu.Unlock()
	if err != nil {
		return fmt.Errorf("помощник вышел: %w", err)
	}
	return fmt.Errorf("помощник закрылся")
}

// fromHelper — что помощник говорит наверх: показал или нет, нажали на
// уведомление, выбрали пункт меню.
func (d *Desk) fromHelper(line []byte) {
	var msg struct {
		Kind   string `json:"вид"`
		ID     int64  `json:"id"`
		OK     bool   `json:"ок"`
		Why    string `json:"почему"`
		Screen string `json:"экран"`
		What   string `json:"что"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return
	}
	switch msg.Kind {
	case "показал":
		d.mu.Lock()
		ch := d.waiting[msg.ID]
		delete(d.waiting, msg.ID)
		d.mu.Unlock()
		if ch != nil {
			ch <- shown{OK: msg.OK, Why: msg.Why}
		}
	case "нажали":
		if d.open != nil {
			d.open(msg.Screen)
		}
	case "пункт":
		switch msg.What {
		case "переподключиться":
			if d.reconnect != nil {
				d.reconnect()
			}
		case "журнал":
			d.open("$журнал")
		}
	}
}
