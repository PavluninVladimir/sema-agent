// Package gate — шлюз: лицо Сёмы в интернете.
//
// Сёма живёт дома за роутером и снаружи недостижим. Шлюз стоит на
// машине с постоянным адресом и доменом, принимает запросы из интернета и
// вносит их к Сёме по уже опознанному проводу.
//
// Соединение открывает **Сёма**, а не шлюз: прячется Сёма, а звонит
// тот, кого не найти по адресу. Роли шифрованного разговора при этом
// прежние — Сёма предъявляет серверный сертификат, шлюз свой,
// выданный его центром, —
// потому что TLS безразлично, кто открыл сокет.
//
// Управление и потоки — разные соединения на **одном** порту. Порт один
// нарочно: дома Сёму изолируют на роутере, и наружу разрешается ровно одно
// направление; диапазон пришлось бы открывать целиком. Кто зачем пришёл,
// Сёма говорит первым кадром.
//
// TLS перед шлюзом завершает край — по умолчанию nginx, который на
// живой машине уже держит 80 и 443 и умеет certbot. Значит шлюз
// слушает петлю и про сертификат на домен не знает ничего. Свой ACME —
// запасной путь для машины, где края нет (см. acme.go).
//
// Чтобы отсеивать, надо видеть: содержимое запросов проходит через эту
// машину в открытом виде. Это названная цена, а не забытая подробность —
// машина шлюза настолько же чувствительна, насколько домашний сервер.
package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"
)

// Первый кадр соединения, которое открыл Сёма: зачем оно. Те же слова, что
// у него в engine/agent_gateway.go — это договор двух сторон.
const (
	KindControl = "управление"
	KindStream  = "поток"
)

// hello — что Сёма говорит первым кадром.
type hello struct {
	Kind   string `json:"вид"`
	Stream string `json:"поток,omitempty"`
}

// Caller — то, что шлюзу нужно от провода к Сёме: попросить
// открыть поток. Интерфейсом, а не самим link.Conn: провод — чужая забота,
// здесь важно только «умеет спросить».
type Caller interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	Live() bool
}

// Config — настройки шлюза. Два адреса, и путать их нельзя: один смотрит в
// интернет, другой — в петлю.
type Config struct {
	// PublicEndpoint — публичный адрес, куда звонит Сёма. Тот единственный порт,
	// который открывают на домашнем роутере.
	PublicEndpoint string
	// BrowserAddr — где принимаются запросы браузеров. По умолчанию петля:
	// перед шлюзом стоит nginx, он и держит край.
	BrowserAddr string
	// Domain — имя, под которым Сёма видна снаружи. Нужен для запасного
	// пути с собственным ACME и для подписи состояния.
	Domain string
	// Edge — кто держит публичный сертификат: «nginx» или «свой ACME».
	Edge string
	// MaxBody — потолок тела обычного запроса; MaxFile — для загрузки
	// файлов. Экран и действие в мегабайты не укладываются только у того,
	// кто хочет занять память.
	MaxBody int64
	MaxFile int64
	// LoginTries — сколько неудачных входов подряд с одного адреса терпим,
	// LoginBan — на сколько закрываем доступ после этого.
	LoginTries int
	LoginBan   time.Duration
	// RateNormal и RateLogin — сколько запросов в минуту с одного
	// адреса. На входе строже: там перебирают, а не работают.
	RateNormal int
	RateLogin  int
}

// Defaults — значения, с которыми шлюз поднимается на машине с
// nginx впереди. Наружного адреса среди них нет нарочно: шлюз,
// случайно поднявшийся на 0.0.0.0, обошёл бы край стороной.
func Defaults() Config {
	return Config{
		BrowserAddr: "127.0.0.1:8770",
		Edge:        "nginx",
		MaxBody:     4 << 20,
		MaxFile:     64 << 20,
		LoginTries:  8,
		LoginBan:    15 * time.Minute,
		RateNormal:  600,
		RateLogin:   30,
	}
}

// Gateway — состояние края.
type Gateway struct {
	cfg    Config
	sema   Caller
	clock  func() time.Time
	filter *filter
	tls    *tlsClient
	acme   *acmeManager // запасной путь: свой сертификат. nil — край держит сосед

	mu      sync.Mutex
	waiting map[string]chan net.Conn // потоки, ожидающие соединения от Сёмы
	streams int
	total   int
	// notDelivered — запросы, которые шлюз пропустил, а внести не смог:
	// Сёма не дозвонился или поток оборвался. Счётчик свой, а не сита:
	// сложить их значило бы показать человеку «отсеяно 500» в тот день, когда
	// край не отсеял ничего, а дома выключили свет. Где чинить — разные места.
	notDelivered int
}

// New — шлюз с этими настройками и этим проводом к Сёме.
func New(cfg Config, sema Caller) *Gateway {
	if cfg.BrowserAddr == "" {
		cfg.BrowserAddr = Defaults().BrowserAddr
	}
	g := &Gateway{cfg: cfg, sema: sema, clock: time.Now, waiting: map[string]chan net.Conn{}}
	g.filter = newFilter(cfg, g.clock)
	return g
}

// ---- Приём звонков Сёмы ----

// AcceptSema слушает публичный порт и разбирает, зачем Сёма позвонил.
// Один порт на всё: и управляющий провод, и каждый поток браузера.
func (g *Gateway) AcceptSema(ctx context.Context, hold func(context.Context, net.Conn) error) error {
	if strings.TrimSpace(g.cfg.PublicEndpoint) == "" {
		return fmt.Errorf("не сказано, где принимать звонки Сёмы (-gate-sema)")
	}
	conf, err := g.tlsForSema()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", g.cfg.PublicEndpoint)
	if err != nil {
		return err
	}
	defer ln.Close()
	log.Printf("шлюз: принимаю звонки Сёмы на %s (взаимная проверка сертификатов)", g.cfg.PublicEndpoint)
	go func() { <-ctx.Done(); _ = ln.Close() }()
	// потолок одновременных рукопожатий: на публичный порт стучатся все
	// кому не лень, и каждый стук не должен стоить горутины навсегда
	slots := make(chan struct{}, 32)
	for {
		raw, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = raw.Close()
			continue
		}
		go func() {
			defer func() { <-slots }()
			g.onCall(ctx, raw, hold, conf)
		}()
	}
}

func (g *Gateway) onCall(ctx context.Context, raw net.Conn, hold func(context.Context, net.Conn) error, conf *tlsClient) {
	conn, err := conf.wrap(ctx, raw)
	if err != nil {
		// стучится кто угодно, а сертификат от центра Сёмы есть только у
		// неё: молчим в журнал одной строкой, без подробностей чужого стука
		_ = raw.Close()
		return
	}
	kind, stream, err := ReadHello(conn)
	if err != nil {
		log.Printf("шлюз: Сёма не сказал, зачем соединение: %v", err)
		_ = conn.Close()
		return
	}
	switch kind {
	case KindControl:
		if err := hold(ctx, conn); err != nil && ctx.Err() == nil {
			log.Printf("шлюз: управляющий провод закрылся: %v", err)
		}
	case KindStream:
		g.mu.Lock()
		ch := g.waiting[stream]
		delete(g.waiting, stream)
		g.mu.Unlock()
		if ch == nil {
			// Сёма открыл поток, которого никто не ждёт: браузер ушёл, пока
			// соединение шло. Закрываем — но говорим об этом, иначе пропажа
			// потоков выглядела бы тишиной
			log.Printf("шлюз: поток %s никто не ждёт — закрываю", stream)
			_ = conn.Close()
			return
		}
		ch <- conn
	default:
		log.Printf("шлюз: непонятное соединение вида %q", kind)
		_ = conn.Close()
	}
}

// ReadHello — первый кадр соединения, без забегания вперёд.
//
// Читать его буферизованным читателем нельзя: буфер утащил бы начало
// потока, и первые байты запроса браузера пропали бы молча. Кадр короткий и
// один на соединение, поэтому байт за байтом здесь ничего не стоит.
func ReadHello(r io.Reader) (kind, stream string, err error) {
	var head []byte
	one := make([]byte, 1)
	for !strings.HasSuffix(string(head), "\r\n\r\n") {
		if len(head) > 512 {
			return "", "", fmt.Errorf("приветствие без конца заголовка")
		}
		if _, err := io.ReadFull(r, one); err != nil {
			return "", "", err
		}
		head = append(head, one[0])
	}
	length := 0
	for _, line := range strings.Split(strings.TrimSpace(string(head)), "\r\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			continue
		}
		if _, err := fmt.Sscanf(strings.TrimSpace(value), "%d", &length); err != nil {
			return "", "", fmt.Errorf("плохая длина приветствия")
		}
	}
	if length <= 0 || length > 1<<16 {
		return "", "", fmt.Errorf("плохая длина приветствия: %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return "", "", err
	}
	var p hello
	if err := json.Unmarshal(body, &p); err != nil {
		return "", "", err
	}
	return p.Kind, p.Stream, nil
}

// ---- Потоки ----

// ErrSemaUnreachable — провода к Сёме нет или поток не пришёл. Отдельная
// ошибка, чтобы браузеру сказать про Сёму, а не про «сбой шлюза»: человек в
// этот миг снаружи и причину иначе не узнает.
var ErrSemaUnreachable = fmt.Errorf("Сёма не отвечает")

// StreamTimeout — сколько ждём, пока Сёма откроет соединение под запрос. Долго
// ждать нельзя: человек снаружи, и «не дозвонились» полезнее, чем белая
// страница через минуту.
const StreamTimeout = 15 * time.Second

// stream — соединение под один запрос браузера. Просим Сёму по управляющему
// проводу и ждём, пока он позвонит; байты дальше идут напрямую.
//
// Своё соединение на поток, а не общий провод: провод пишет кадры в один
// поток под замком, и одно скачивание вложения заткнуло бы и пинги, и все
// остальные запросы. А живое обновление в «запрос — ответ» не укладывается
// вовсе.
func (g *Gateway) stream(ctx context.Context, _, _ string) (net.Conn, error) {
	if g.sema == nil || !g.sema.Live() {
		return nil, fmt.Errorf("%w: управляющего провода нет — он звонит сам, значит пока не дозвонился", ErrSemaUnreachable)
	}
	token := newStreamToken()
	ch := make(chan net.Conn, 1)
	g.mu.Lock()
	g.waiting[token] = ch
	g.streams++
	g.total++
	g.mu.Unlock()
	drop := func() {
		g.mu.Lock()
		delete(g.waiting, token)
		g.streams--
		g.mu.Unlock()
	}
	cctx, cancel := context.WithTimeout(ctx, StreamTimeout)
	defer cancel()
	if _, err := g.sema.Call(cctx, "шлюз.поток", map[string]any{"поток": token}); err != nil {
		drop()
		return nil, fmt.Errorf("%w: %v", ErrSemaUnreachable, err)
	}
	select {
	case conn := <-ch:
		// счётчик держим до закрытия соединения: «потоков сейчас» должно
		// значить именно это
		return &countedStream{Conn: conn, done: drop}, nil
	case <-cctx.Done():
		drop()
		return nil, fmt.Errorf("%w: соединение под запрос не пришло за %s", ErrSemaUnreachable, StreamTimeout)
	}
}

// countedStream — соединение, которое уменьшает счётчик, когда закрылось.
type countedStream struct {
	net.Conn
	once sync.Once
	done func()
}

func (p *countedStream) Close() error {
	p.once.Do(p.done)
	return p.Conn.Close()
}

// ---- Браузеры ----

// Handler — весь путь запроса из интернета: сначала сито, потом внос к
// Сёме. Порядок именно такой: то, чего у него нет, до него не доходит вовсе.
func (g *Gateway) Handler() http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(p *httputil.ProxyRequest) {
			// хост в адресе не значит ничего: соединение приходит от Сёмы, и
			// транспорт зовёт наш dial, а не разрешает имя
			p.Out.URL.Scheme = "http"
			p.Out.URL.Host = "сёма"
			p.Out.Host = p.In.Host
			// Настоящий адрес обратившегося ставим сами, а не SetXForwarded:
			// тот берёт его из RemoteAddr, а за nginx это всегда петля — в
			// журнале доступа Сёмы оказался бы один адрес у всех.
			p.Out.Header.Set("X-Forwarded-For", RealAddr(p.In))
			p.Out.Header.Set("X-Forwarded-Proto", scheme(p.In))
			p.Out.Header.Set("X-Forwarded-Host", p.In.Host)
		},
		Transport: &http.Transport{
			DialContext: g.stream,
			// соединения не переиспользуем: каждое — свой поток до Сёмы, и
			// держать их простаивающими значит держать открытыми сокеты
			// через полмира
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: 2 * time.Minute,
		},
		// потоково: сотня мегабайт вложения не должна оказаться в памяти
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			g.mu.Lock()
			g.notDelivered++
			g.mu.Unlock()
			log.Printf("шлюз: %s %s не внесён: %v", r.Method, r.URL.Path, err)
			troublePage(w, r, err)
		},
	}
	return g.filter.wrap(proxy)
}

// scheme — по чему пришёл запрос. За nginx об этом знает он; без него — мы
// сами, потому что своим ACME мы и держим TLS.
func scheme(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" && fromEdge(r) {
		return v
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// fromEdge — запрос пришёл от своего края (nginx на этой же машине), значит
// его заголовкам можно верить. Со стороны интернета их подделывают.
func fromEdge(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// RealAddr — кто обратился на самом деле.
//
// От своего края берём первую запись цепочки: nginx дописывает клиента в
// начало, и других прокси перед ним нет. Из интернета — только адрес
// соединения: заголовок оттуда принадлежит тому, кто его прислал.
func RealAddr(r *http.Request) string {
	if fromEdge(r) {
		if f := r.Header.Get("X-Forwarded-For"); f != "" {
			if first := strings.TrimSpace(strings.Split(f, ",")[0]); first != "" {
				return first
			}
		}
		if f := strings.TrimSpace(r.Header.Get("X-Real-IP")); f != "" {
			return f
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// troublePage — внятный ответ человеку, который сейчас снаружи. Пустая
// страница и висящий запрос здесь недопустимы: причину он иначе не узнает,
// а причина бывает в его силах — закрытое правило на роутере, спящая Сёма.
func troublePage(w http.ResponseWriter, r *http.Request, err error) {
	code := http.StatusBadGateway
	title := "Сёма не отвечает"
	why := "Шлюз на связи, а Сёма до него не дозвонился. " +
		"Соединение открывает Сёма: проверьте, работает ли он дома и разрешён ли ему выход на этот адрес."
	if err != nil && !strings.Contains(err.Error(), ErrSemaUnreachable.Error()) {
		why = "Шлюз на связи, а внести запрос не вышло: " + err.Error()
	}
	// htmx подставит ответ любого кода в окно, поэтому для него — короткая
	// разметка, а не страница целиком
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(code)
		fmt.Fprintf(w, `<div class="ожидание">%s. %s</div>`, title, why)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": title + ": " + why, "шлюз": "жив"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><meta charset=utf-8><title>%s</title>
<style>body{font:16px/1.5 system-ui;margin:10vh auto;max-width:36em;padding:0 1em}h1{font-size:20px}p{color:#555}</style>
<h1>%s</h1><p>%s</p><p>Шлюз работает — беда не на нём.</p>`, title, title, why)
}

// ---- Что рассказать Сёме о себе ----

// Status — ответ на «шлюз.состояние». Сёма показывает это на
// экране «Агенты», и каждая строка подписана как его: человек должен
// понимать, где чинить.
func (g *Gateway) Status() map[string]any {
	g.mu.Lock()
	streams, total, notDelivered := g.streams, g.total, g.notDelivered
	g.mu.Unlock()
	fs := g.filter.stats()
	cert, level := "за краем — срок смотрите там, где выписан сертификат", "ok"
	if g.acme != nil {
		cert, level = g.acme.Caption(g.clock())
	}
	return map[string]any{
		"домен": g.cfg.Domain, "край": g.cfg.Edge,
		"сертификат": cert, "уровень": level,
		"браузерам": g.cfg.BrowserAddr, "сёме": g.cfg.PublicEndpoint,
		"пропущено": fs.Passed, "отсеяно": fs.Filtered, "закрыто": fs.Banned,
		"потоков": streams, "всего": total, "не внесено": notDelivered,
	}
}

// Handlers — что Сёма может спросить у шлюза по проводу.
func (g *Gateway) Handlers() map[string]func(context.Context, json.RawMessage) (any, error) {
	return map[string]func(context.Context, json.RawMessage) (any, error){
		"шлюз.состояние": func(context.Context, json.RawMessage) (any, error) {
			return g.Status(), nil
		},
	}
}

// Listen — принимать браузеры. За краем это петля и обычный HTTP; со своим
// ACME — 443 и его сертификат (acme.go).
func (g *Gateway) Listen(ctx context.Context) error {
	srv := &http.Server{
		Addr:              g.cfg.BrowserAddr,
		Handler:           g.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
		// тело и ответ по сроку не режем: загрузка вложения и провод живого
		// обновления живут дольше любого разумного предела
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if g.acme != nil {
		return g.acme.Listen(ctx, srv)
	}
	log.Printf("шлюз: принимаю браузеры на %s (край держит %s)", g.cfg.BrowserAddr, g.cfg.Edge)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
