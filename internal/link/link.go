package link

// Связь агента с Сёмой. Звонит агент: ноутбук засыпает, меняет сеть и
// стоит за домашним роутером, а у Сёмы адрес постоянный.
//
// Пропуск — сертификат, выданный Сёмой по одноразовому приглашению.
// Общий токен в переменной среды пропуском быть перестаёт: его нельзя
// отозвать у одного агента, не сменив всем.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Identity — то, чем агент доказывает, что он это он.
type Identity struct {
	Name string
	// Role — роль агента (житель машины / шлюз). Живёт рядом с
	// удостоверением, а не в самом сертификате: сертификат перевыпускается
	// по сроку, и завязывать на него разрешение нельзя (design.md,
	// решение 1). Пусто — старое удостоверение, заведённое до ролей; по
	// плану переезда такое читается как «житель машины» (он им и был).
	Role string
	Cert string // выданный Сёмой
	Key  string // свой, Сёме не показывался
	Root string // корень Сёмы: по нему агент проверяет, что говорит с ним
}

// Load читает удостоверение из папки; пусто — его ещё нет.
func Load(dir string) (Identity, bool) {
	read := func(name string) string {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return ""
		}
		return string(raw)
	}
	id := Identity{Name: strings.TrimSpace(read("name")), Role: strings.TrimSpace(read("role")),
		Cert: read("cert.pem"), Key: read("key.pem"), Root: read("root.pem")}
	return id, id.Cert != "" && id.Key != "" && id.Root != ""
}

// Save кладёт удостоверение рядом, доступным только хозяину: закрытый
// ключ агента не должен читаться кем попало.
func Save(dir string, id Identity) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for name, body := range map[string]string{
		"name": id.Name, "role": id.Role, "cert.pem": id.Cert, "key.pem": id.Key, "root.pem": id.Root,
	} {
		mode := os.FileMode(0o600)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
			return err
		}
	}
	return nil
}

// Enroll — вход по одноразовому коду: агент делает ключ, отправляет
// открытую часть вместе с ролью, которую просит для себя, и получает
// сертификат вместе с корнем Сёмы. Роль, которую код нёс на самом деле
// (человек выбрал её при выдаче кода), Сёма вправе вернуть в ответе —
// тогда она главнее запрошенной: код выдавал он, а не эта машина.
func Enroll(semaURL, code, role string, can []string) (Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return Identity{}, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return Identity{}, err
	}
	body, _ := json.Marshal(map[string]any{"code": code, "pub": pubDER, "can": can, "role": role})
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Post(
		strings.TrimSuffix(semaURL, "/")+"/api/agents/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		return Identity{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("Сёма не принял: %s", strings.TrimSpace(string(raw)))
	}
	var out struct {
		Cert string `json:"cert"`
		Root string `json:"root"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Identity{}, err
	}
	gotRole := role
	if strings.TrimSpace(out.Role) != "" {
		gotRole = strings.TrimSpace(out.Role)
	}
	name := commonName(out.Cert)
	return Identity{Name: name, Role: gotRole, Cert: out.Cert, Key: string(pem.EncodeToMemory(
		&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), Root: out.Root}, nil
}

func commonName(certPEM string) string {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return ""
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return ""
	}
	return cert.Subject.CommonName
}

// Handler — что агент умеет: имя способа и что он делает.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Conn — соединение с Сёмой: агент звонит, держит его, отвечает на
// запросы Сёмы и сам её спрашивает по тому же проводу.
//
// Кто кому отвечает, решает не номер, а наличие метода: кадр с методом —
// запрос, кадр без метода — ответ. Ответ всегда возвращается той стороне,
// которая запрос задала, поэтому номера двух сторон не путаются, даже
// когда совпадают.
type Conn struct {
	id       Identity
	addr     string
	can      []string
	role     string // своя роль, как её видит сама машина — Сёма сверяет с той, что помнит
	version  string // версия агента: обновление не должно быть невидимым
	handlers map[string]Handler

	sendMu sync.Mutex
	send   func(any) error // отправитель живого соединения; nil — связи нет
	live   io.Closer       // само соединение: его закрывают, когда человек просит перезвонить

	waitMu  sync.Mutex
	waiting map[int]waiter
	nextID  int

	// PingEvery — как часто спрашивать «ты здесь?», PingWait — сколько
	// ждать ответа. Молчание дольше PingWait закрывает провод: мёртвое
	// соединение иначе живёт до таймаута TCP, а это четверть часа, пока
	// в нём висят неподтверждённые байты. Нуль — две минуты и полминуты.
	PingEvery time.Duration
	PingWait  time.Duration

	// OnState — связь появилась или пропала. Нужен рабочему столу: значок
	// у часов говорит ровно об этом. Зовётся из горутины провода, поэтому
	// обработчик не должен в нём задерживаться.
	OnState func(up bool, why string)
}

// sayState — сказать наверх, что со связью. Молчание тоже событие: без
// него значок у часов застывает в последнем виде.
func (c *Conn) sayState(up bool, why string) {
	if c.OnState != nil {
		c.OnState(up, why)
	}
}

// Reconnect — оборвать нынешнее соединение, чтобы позвонить заново.
// Человек нажал «Переподключиться» и ждёт немедленной попытки, а не
// конца растущей паузы.
func (c *Conn) Reconnect() {
	c.sendMu.Lock()
	live := c.live
	c.sendMu.Unlock()
	if live != nil {
		_ = live.Close()
	}
}

// Live — есть ли связь прямо сейчас.
func (c *Conn) Live() bool {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.send != nil
}

// reply — что вернулось на наш запрос: тело или причина отказа.
type reply struct {
	result json.RawMessage
	err    error
}

// waiter — ждущий ответа и соединение, по которому ушёл его вопрос. Ответ
// приходит только тем же соединением: закрылось оно — ждать нечего, даже
// если рядом уже живёт новое.
type waiter struct {
	ch  chan reply
	via io.Closer
}

func New(id Identity, addr string, can []string, role, version string, handlers map[string]Handler) *Conn {
	return &Conn{id: id, addr: addr, can: can, role: role, version: version, handlers: handlers,
		waiting: map[int]waiter{}}
}

// Handle — добавить обработчик после сборки соединения: способности
// собираются в разных местах, а провод один.
func (c *Conn) Handle(method string, h Handler) {
	if c.handlers == nil {
		c.handlers = map[string]Handler{}
	}
	c.handlers[method] = h
}

// ErrNoLink — связи с Сёмой сейчас нет. Отдельная ошибка, чтобы вызывающий
// мог сказать человеку про связь, а не про пустой ответ.
var ErrNoLink = fmt.Errorf("нет связи с Сёмой")

// Call — спросить Сёму по тому же проводу. Отвечает он только на то, что
// человек разрешил этому агенту; неразрешённое возвращает ошибкой, не
// разрывая соединение.
func (c *Conn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	// в карту ожидающих встаём под тем же замком, под которым читаем
	// отправителя: иначе обрыв успевает разослать отказы до нашей
	// записи, и вопрос повисает до дедлайна вместо честного «нет связи»
	c.sendMu.Lock()
	send := c.send
	var id int
	ch := make(chan reply, 1)
	if send != nil {
		id = c.await(c.live, ch)
	}
	c.sendMu.Unlock()
	if send == nil {
		return nil, ErrNoLink
	}
	defer c.forget(id)
	if err := send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoLink, err)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.result, r.err
	}
}

// await — встать в ожидающие ответа на вопрос, уходящий по соединению via.
func (c *Conn) await(via io.Closer, ch chan reply) int {
	c.waitMu.Lock()
	defer c.waitMu.Unlock()
	c.nextID++
	c.waiting[c.nextID] = waiter{ch: ch, via: via}
	return c.nextID
}

func (c *Conn) forget(id int) {
	c.waitMu.Lock()
	delete(c.waiting, id)
	c.waitMu.Unlock()
}

// deliver — ответ Сёмы своему ожидающему. Чужой номер молча роняем: это
// либо запоздавший ответ на отменённый запрос, либо чужая беседа. Ответ,
// пришедший не тем соединением, которым ушёл вопрос, тоже чужой: номера у
// двух соединений общие, а беседы разные.
func (c *Conn) deliver(from io.Closer, id int, r reply) {
	c.waitMu.Lock()
	w, ok := c.waiting[id]
	if ok && w.via == from {
		delete(c.waiting, id)
	}
	c.waitMu.Unlock()
	if ok && w.via == from {
		w.ch <- r
	}
}

// failWaiting — соединение закрылось: ждущие ответа по нему узнают сразу, а
// не по таймауту. Вопросы, ушедшие другим соединением, не трогаем: у них
// ответ ещё может прийти.
func (c *Conn) failWaiting(via io.Closer, why error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.waitMu.Lock()
	var gone []chan reply
	for id, w := range c.waiting {
		if w.via == via {
			gone = append(gone, w.ch)
			delete(c.waiting, id)
		}
	}
	c.waitMu.Unlock()
	for _, ch := range gone {
		ch <- reply{err: why}
	}
}

// Run держит связь: оборвалось — ждём и звоним снова. Сёма может
// перезагружаться, ноутбук — засыпать; это обычная жизнь, а не ошибка.
func (c *Conn) Run(ctx context.Context) {
	wait := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		if err := c.once(ctx); err != nil && ctx.Err() == nil {
			log.Printf("связь с Сёмой: %v (попробую через %s)", err, wait.Round(time.Second))
			c.sayState(false, err.Error())
		}
		// пауза растёт, пока Сёмы нет, но соединение, которое пожило,
		// сбрасывает её: иначе после ночи сна ноутбука первый же выкат
		// Сёмы стоил бы минуты молчания, хотя он поднялся за шесть
		// секунд
		if time.Since(started) > time.Minute {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait < time.Minute {
			wait *= 2
		}
	}
}

// TLSConfig — чем агент доказывает, что он это он, и как проверяет Сёму.
// Одна и та же настройка на оба случая: и когда сокет открывает агент, и
// когда его открывает Сёма (шлюз). Роли шифрованного разговора от
// направления TCP не зависят: агент всегда клиент TLS, Сёма всегда сервер.
func (c *Conn) TLSConfig() (*tls.Config, error) {
	cert, err := tls.X509KeyPair([]byte(c.id.Cert), []byte(c.id.Key))
	if err != nil {
		return nil, fmt.Errorf("удостоверение не читается: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(c.id.Root)) {
		return nil, fmt.Errorf("корень Сёмы не читается")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,   // проверяем и её: разговор должен быть с Сёмой
		ServerName:   "sema", // так Сёма называет себя в своём сертификате
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// Hold — провод на уже открытом соединении. Нужен шлюзу: там TCP
// открывает Сёма, потому что прячется он, а не шлюз. Всё остальное —
// приветствие, кадры, пинг, обработчики — то же самое, и нарочно: два
// разных провода означали бы два разных набора граблей.
//
// Возвращается, когда соединение закрылось. Переподключаться шлюз не
// пытается: звонить будет Сёма.
func (c *Conn) Hold(ctx context.Context, conn net.Conn) error {
	return c.serve(ctx, conn, "соединение открыл Сёма")
}

func (c *Conn) once(ctx context.Context) error {
	conf, err := c.TLSConfig()
	if err != nil {
		return err
	}
	d := &tls.Dialer{Config: conf}
	conn, err := dialIPv4First(ctx, d, c.addr)
	if err != nil {
		return err
	}
	return c.serve(ctx, conn, c.addr)
}

// serve — сам разговор поверх готового соединения, кто бы его ни открыл.
//
// Провод у агента один, и он — последний открытый. Шлюзу Сёма звонит
// заново, когда своё соединение счёл мёртвым, а шлюз мог этого ещё не
// заметить: 5 октября 2026 после ночного обрыва старое соединение
// закрылось через минуты после нового и, уходя, стёрло новое — шлюз
// отвечал 502 при живом проводе, пока службу не перезапустили. Поэтому
// новое соединение закрывает прежнее, а уходящее чистит за собой только
// если оно всё ещё текущее.
func (c *Conn) serve(ctx context.Context, conn net.Conn, from string) error {
	defer conn.Close()
	log.Printf("связь с Сёмой установлена: %s", from)
	done := make(chan struct{})
	defer close(done)

	out := bufio.NewWriter(conn)
	// В провод пишут разом: пинг из своей горутины и ответы обработчиков,
	// каждый в своей. Без замка кадры перемешиваются, и Сёма получает
	// битый JSON-RPC — на её стороне такой замок есть, здесь его не было.
	var writeMu sync.Mutex
	send := func(v any) error {
		body, err := json.Marshal(v)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		// срок записи: в мёртвое соединение с полным буфером сокета запись
		// встала бы до таймаута TCP и держала бы замок — и пинг за ним
		_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
		defer conn.SetWriteDeadline(time.Time{})
		if _, err := fmt.Fprintf(out, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
			return err
		}
		return out.Flush()
	}
	// с этого мгновения агент может и сам спрашивать Сёму
	c.sendMu.Lock()
	prev := c.live
	c.live, c.send = conn, send
	c.sendMu.Unlock()
	if prev != nil {
		log.Printf("связь с Сёмой: пришло новое соединение — прежнее закрываю")
		_ = prev.Close()
	}
	c.sayState(true, "")
	defer func() {
		c.sendMu.Lock()
		current := c.live == conn
		if current {
			c.send, c.live = nil, nil
		}
		c.sendMu.Unlock()
		c.failWaiting(conn, ErrNoLink)
		if current {
			c.sayState(false, "")
		}
	}()

	// первым делом рассказываем, что умеем, кто мы и какой роли: разрешает
	// способности человек, а роль сверяет сама Сёма с той, что запомнила
	// при выдаче кода — несовпадение обязано быть отказом, а не тихим
	// принятием чужого слова (spec: «роль, не совпавшая с запомненной»)
	if err := send(map[string]any{"jsonrpc": "2.0", "method": "hello",
		"params": map[string]any{"can": c.can, "name": c.id.Name, "role": c.role, "version": c.version}}); err != nil {
		return err
	}
	go c.keepAlive(ctx, conn, send, done)

	r := bufio.NewReaderSize(conn, 1<<20)
	for {
		body, err := readFramed(r)
		if err != nil {
			return err
		}
		var req struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &req) != nil || req.ID == nil {
			continue
		}
		// кадр без метода — ответ на наш вопрос, а не вопрос к нам
		if req.Method == "" {
			r := reply{result: req.Result}
			if req.Error != nil {
				r.err = fmt.Errorf("%s", req.Error.Message)
			}
			c.deliver(conn, *req.ID, r)
			continue
		}
		h, ok := c.handlers[req.Method]
		if !ok {
			_ = send(map[string]any{"jsonrpc": "2.0", "id": *req.ID,
				"error": map[string]any{"code": -32601, "message": "агент не умеет " + req.Method}})
			continue
		}
		go func(id int, params json.RawMessage) {
			cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			res, err := h(cctx, params)
			if err != nil {
				_ = send(map[string]any{"jsonrpc": "2.0", "id": id,
					"error": map[string]any{"code": -32000, "message": err.Error()}})
				return
			}
			_ = send(map[string]any{"jsonrpc": "2.0", "id": id, "result": res})
		}(*req.ID, req.Params)
	}
}

// writeWait — сколько ждём, пока Сёма вычитает кадр (у Сёмы тот же срок).
const writeWait = 10 * time.Second

// keepAlive — редкий пинг с ответом. Сёме он говорит, что агент ещё здесь,
// а агенту — что Сёма ещё там: без ответа мёртвое соединение выглядит
// живым, пока его не убьёт TCP, и шлюз всё это время отдаёт 502. Любой
// ответ, даже отказ, значит «провод жив»; молчание дольше PingWait —
// провод закрывается, и Сёма (или Run у жителя машины) звонит заново.
func (c *Conn) keepAlive(ctx context.Context, conn net.Conn, send func(any) error, done <-chan struct{}) {
	every, wait := c.PingEvery, c.PingWait
	if every <= 0 {
		every = 2 * time.Minute
	}
	if wait <= 0 {
		wait = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-t.C:
		}
		ch := make(chan reply, 1)
		id := c.await(conn, ch)
		if err := send(map[string]any{"jsonrpc": "2.0", "id": id, "method": "ping"}); err != nil {
			c.forget(id)
			return
		}
		timer := time.NewTimer(wait)
		select {
		case r := <-ch:
			timer.Stop()
			if r.err == ErrNoLink {
				return // соединение закрылось само
			}
		case <-timer.C:
			c.forget(id)
			log.Printf("связь с Сёмой: на пинг нет ответа %s — закрываю соединение", wait)
			_ = conn.Close()
			return
		case <-done:
			timer.Stop()
			c.forget(id)
			return
		case <-ctx.Done():
			timer.Stop()
			c.forget(id)
			return
		}
	}
}

func readFramed(r *bufio.Reader) ([]byte, error) {
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
	if length <= 0 || length > 32<<20 {
		return nil, fmt.Errorf("плохая длина сообщения")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// dialIPv4First — сначала по IPv4, потом как получится. Под launchd имя
// «sema.local» разрешалось в link-local IPv6 (fe80::…%en0), по которому нет
// маршрута, и агент бесконечно пробовал только его. Из обычной оболочки то
// же имя давало IPv4 и всё работало — то есть падало ровно там, где никто
// не смотрит.
func dialIPv4First(ctx context.Context, d *tls.Dialer, addr string) (net.Conn, error) {
	conn, err := d.DialContext(ctx, "tcp4", addr)
	if err == nil {
		return conn, nil
	}
	conn2, err2 := d.DialContext(ctx, "tcp", addr)
	if err2 == nil {
		return conn2, nil
	}
	return nil, err // первая ошибка честнее: она про IPv4, который и ждали
}
