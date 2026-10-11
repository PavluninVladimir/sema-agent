package gate

// Шлюз целиком: браузер стучится в него, он отсеивает или просит у
// Сёмы поток и вносит запрос. Сёма здесь поддельный, но провод, TLS и
// приветствие настоящие — иначе проверялось бы намерение, а не поведение.

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSema — тот, кто звонит шлюзу. На просьбу открыть поток
// открывает соединение к его публичному порту, говорит приветствие и
// переливает байты в свой HTTP-сервер: ровно то, что делает настоящий.
type fakeSema struct {
	t        *testing.T
	certs    *testCerts
	addr     string // куда звонить: публичный порт шлюза
	upstream string // куда вносить запросы: адрес поддельного HTTP Сёмы
	alive    bool
	silent   bool // принимает просьбу и не звонит: так проверяется срок

	mu       sync.Mutex
	requests int
}

func (s *fakeSema) Live() bool { return s.alive }

func (s *fakeSema) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if method != "шлюз.поток" {
		return nil, fmt.Errorf("поддельный Сёма не умеет %q", method)
	}
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()
	raw, _ := json.Marshal(params)
	var p struct {
		Stream string `json:"поток"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if s.silent {
		return json.RawMessage(`{"ок":true}`), nil
	}
	// звоним в своей горутине и отвечаем сразу — как настоящий: иначе ответ
	// держал бы читателя управляющего провода, пока идёт перекачка
	go s.dial(p.Stream)
	return json.RawMessage(`{"ок":true}`), nil
}

func (s *fakeSema) dial(stream string) {
	raw, err := net.DialTimeout("tcp", s.addr, 5*time.Second)
	if err != nil {
		return
	}
	outward := tls.Server(raw, s.certs.semaTLS())
	if err := outward.Handshake(); err != nil {
		_ = outward.Close()
		return
	}
	body, _ := json.Marshal(hello{Kind: KindStream, Stream: stream})
	if _, err := fmt.Fprintf(outward, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		_ = outward.Close()
		return
	}
	inward, err := net.DialTimeout("tcp", s.upstream, 5*time.Second)
	if err != nil {
		_ = outward.Close()
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	pour := func(src, dst net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
			return
		}
		_ = dst.Close()
	}
	go pour(outward, inward)
	go pour(inward, outward)
	wg.Wait()
	_ = outward.Close()
	_ = inward.Close()
}

// harness — шлюз с поддельным Сёмой и поддельным его HTTP наверху.
type harness struct {
	g        *Gateway
	sema     *fakeSema
	edge     *httptest.Server // «nginx»: через него ходит браузер
	upstream *httptest.Server // «Сёма»: то, во что вносятся запросы
}

func newHarness(t *testing.T, handler http.Handler, tweak func(*Config)) *harness {
	t.Helper()
	certs := newCerts(t)
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	public := ln.Addr().String()
	_ = ln.Close()

	sema := &fakeSema{t: t, certs: certs, addr: public,
		upstream: strings.TrimPrefix(upstream.URL, "http://"), alive: true}

	cfg := Defaults()
	cfg.PublicEndpoint = public
	cfg.BrowserAddr = "127.0.0.1:0" // браузеров принимаем через httptest, а не сами
	cfg.Domain = "sema.example.org"
	if tweak != nil {
		tweak(&cfg)
	}
	g := New(cfg, sema)
	g.TLS(certs.edgeTLS())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready := make(chan struct{})
	go func() {
		close(ready)
		// управляющий провод в этих проверках не нужен: Live() поддельный
		// Сёма говорит сам, а потоки приходят на тот же порт
		if err := g.AcceptSema(ctx, func(context.Context, net.Conn) error { return nil }); err != nil && ctx.Err() == nil {
			t.Logf("приём звонков Сёмы: %v", err)
		}
	}()
	<-ready
	waitPort(t, public)

	edge := httptest.NewServer(g.Handler())
	t.Cleanup(edge.Close)
	return &harness{g: g, sema: sema, edge: edge, upstream: upstream}
}

func waitPort(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("шлюз не поднял %s", addr)
}

// echoSema — поддельный Сёма наверху: говорит, что получил.
func echoSema() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"путь": r.URL.Path, "тело": string(body),
			"адрес": r.Header.Get("X-Forwarded-For"), "схема": r.Header.Get("X-Forwarded-Proto"),
		})
	})
}

// Обычная переписка по-русски проходит нетронутой: содержимое шлюз не
// судит. Кавычки, код и русский текст — то, на чём ломались бы образцы
// вредоносности, которых мы нарочно не завели.
func TestPlainRussianChatPassesUntouched(t *testing.T) {
	env := newHarness(t, echoSema(), nil)
	text := `Сёма, посмотри: select * from writes where "имя" = 'Вова'; ` +
		"```go\nfor i := 0; i < 10; i++ { /* «кавычки» */ }\n```" +
		` и <script>alert(1)</script> — это цитата из письма, а не приказ`
	body, _ := json.Marshal(map[string]any{"action": "send_message", "params": map[string]string{"text": text}})
	req, _ := http.NewRequest("POST", env.edge.URL+"/api/action", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("запрос не прошёл: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("шлюз не пропустил обычную переписку: %d %s", resp.StatusCode, raw)
	}
	// ключи ответа по-русски: имена полей латиницей, а связь с ключом —
	// тегом, иначе разбор молча перестал бы совпадать
	var got struct {
		Path string `json:"путь"`
		Body string `json:"тело"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Path != "/api/action" {
		t.Fatalf("путь доехал не тот: %q", got.Path)
	}
	if got.Body != string(body) {
		t.Fatalf("тело изменилось по дороге:\nбыло: %s\nстало: %s", body, got.Body)
	}
}

// Стук по несуществующему пути до Сёмы не доходит вовсе и виден в счётчиках.
func TestUnknownPathNeverReachesSema(t *testing.T) {
	var arrived atomic.Int32
	env := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived.Add(1)
		w.WriteHeader(200)
	}), nil)
	for _, path := range []string{"/wp-login.php", "/.env", "/api/выдумка", "/admin",
		"/api/agents/enroll", "/api/", "/письмо/"} {
		resp, err := http.Get(env.edge.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: ждали отказ шлюза, получили %d", path, resp.StatusCode)
		}
	}
	if n := arrived.Load(); n != 0 {
		t.Fatalf("до Сёмы дошло %d чужих запросов — отсев не на краю", n)
	}
	if n := env.g.Status()["отсеяно"].(int); n < 7 {
		t.Fatalf("отсеяно посчитано как %d, а стучались семь раз", n)
	}
	if n := env.sema.requests; n != 0 {
		t.Fatalf("шлюз просил у Сёмы %d потоков под чужие пути", n)
	}
	// а знакомый путь проходит — иначе тест проверял бы «не пускает ничего»
	resp, err := http.Get(env.edge.URL + "/api/ui?screen=health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("знакомый путь тоже не прошёл: %d", resp.StatusCode)
	}
}

// Перебор входа закрывает адрес на время, и это видно в счётчиках.
func TestLoginBruteForceBansAddr(t *testing.T) {
	env := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "нужен вход", http.StatusUnauthorized)
	}), func(cfg *Config) { cfg.LoginTries = 3; cfg.LoginBan = time.Minute })

	for i := 0; i < 3; i++ {
		resp, err := http.Post(env.edge.URL+"/api/auth/login/finish", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("попытка %d: ждали отказ Сёмы, получили %d", i+1, resp.StatusCode)
		}
	}
	// четвёртая упирается в край, а не в Сёму
	resp, err := http.Post(env.edge.URL+"/api/auth/login/finish", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("после трёх неудач адрес не закрыт: %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("не сказано, когда пробовать снова")
	}
	if n := env.g.Status()["закрыто"].(int); n != 1 {
		t.Fatalf("закрытых адресов %d, а закрывали один", n)
	}
	// и обычный путь этому адресу тоже закрыт: бан по адресу, не по пути
	resp2, err := http.Get(env.edge.URL + "/api/ui")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("закрытый адрес ходит другими путями: %d", resp2.StatusCode)
	}
}

// Предел частоты на входе строже обычного: там перебирают, а не работают.
func TestLoginRateLimitIsStricter(t *testing.T) {
	env := newHarness(t, echoSema(), func(cfg *Config) {
		cfg.RateLogin = 2
		cfg.RateNormal = 50
		cfg.LoginTries = 0
	})
	codes := func(path string, times int) []int {
		var out []int
		for i := 0; i < times; i++ {
			resp, err := http.Get(env.edge.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			out = append(out, resp.StatusCode)
		}
		return out
	}
	// три захода на страницу входа: третий отсеян
	got := codes("/login", 3)
	if got[0] != 200 || got[1] != 200 || got[2] != http.StatusTooManyRequests {
		t.Fatalf("предел входа не сработал: %v", got)
	}
	// а обычные экраны считаются своим, куда более щедрым пределом
	for i, code := range codes("/api/ui", 5) {
		if code != 200 {
			t.Fatalf("обычный путь отсеян на %d-м запросе (%d) — предел входа задел не то", i+1, code)
		}
	}
}

// Настоящий адрес обратившегося: от своего края верим его заголовку, из
// интернета — только адресу соединения.
func TestRealAddr(t *testing.T) {
	env := newHarness(t, echoSema(), nil)
	req, _ := http.NewRequest("GET", env.edge.URL+"/api/ui", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Addr   string `json:"адрес"`
		Scheme string `json:"схема"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	// httptest ходит по петле — то же, что nginx на этой же машине
	if got.Addr != "203.0.113.9" {
		t.Fatalf("настоящий адрес до Сёмы не доехал: %q", got.Addr)
	}
	if got.Scheme != "https" {
		t.Fatalf("схема от края не доехала: %q", got.Scheme)
	}
	// а тот же заголовок от неместного адреса не в счёт: его прислал тот,
	// кому верить нельзя
	foreign := httptest.NewRequest("GET", "/api/ui", nil)
	foreign.RemoteAddr = "203.0.113.200:44321"
	foreign.Header.Set("X-Forwarded-For", "10.0.0.1")
	if addr := RealAddr(foreign); addr != "203.0.113.200" {
		t.Fatalf("заголовку из интернета поверили: %q", addr)
	}
}

// Сёма недоступен — человек снаружи получает внятный ответ, а не пустую
// страницу и не висящий запрос: причина бывает в его силах.
func TestSemaUnreachableGivesClearAnswer(t *testing.T) {
	env := newHarness(t, echoSema(), nil)
	env.sema.alive = false
	started := time.Now()
	resp, err := http.Get(env.edge.URL + "/api/ui?screen=health")
	if err != nil {
		t.Fatalf("запрос оборвался вместо внятного ответа: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("ждали 502 «Сёма не отвечает», получили %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "Сёма не отвечает") {
		t.Fatalf("ответ не называет причину: %s", raw)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatalf("ответ шёл %s — человек снаружи столько не ждёт", time.Since(started).Round(time.Millisecond))
	}
	// страница браузеру — разметкой, а не JSON: он её читает глазами
	resp2, err := http.Get(env.edge.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	page, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(page), "<h1>") || !strings.Contains(string(page), "Шлюз работает") {
		t.Fatalf("страница беды не объясняет, кто цел: %s", page)
	}
	// И в счётчиках это не отсев: край не отклонил ни одного запроса, он их
	// пропустил и не смог внести. Сложить два числа значило бы показать
	// человеку «отсеяно 500» в тот день, когда чинить надо дома.
	st := env.g.Status()
	if n := st["отсеяно"].(int); n != 0 {
		t.Fatalf("Сёма молчал, а у края «отсеяно %d» — человек пойдёт чинить не туда", n)
	}
	if n := st["не внесено"].(int); n < 2 {
		t.Fatalf("не внесли два запроса, а сказано про %d", n)
	}
	if n := st["пропущено"].(int); n < 2 {
		t.Fatalf("сито пропустило два запроса, а сказано про %d", n)
	}
}

// Сёма принял просьбу и не позвонил — шлюз не висит вечно, а
// говорит, что соединения не пришло.
func TestStreamNeverCameTimeoutNamed(t *testing.T) {
	env := newHarness(t, echoSema(), nil)
	env.sema.silent = true
	// срок потока в проверке ждать целиком незачем: просим напрямую и
	// оборваем ожидание своим контекстом — так же, как это сделает браузер
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	_, err := env.g.stream(ctx, "tcp", "сёма:80")
	if err == nil {
		t.Fatal("поток взялся из воздуха, хотя Сёма не звонил")
	}
	if !strings.Contains(err.Error(), ErrSemaUnreachable.Error()) {
		t.Fatalf("причина не названа: %v", err)
	}
	// и счётчик открытых потоков вернулся к нулю: иначе «потоков сейчас»
	// копило бы неудачи и рано или поздно упёрлось бы в потолок
	if n := env.g.Status()["потоков"].(int); n != 0 {
		t.Fatalf("после неудачи осталось %d открытых потоков", n)
	}
}

// Живое обновление через шлюза: это постоянно открытый провод, а не
// запрос с ответом. Проверяем сменой протокола и кадром в обе стороны.
func TestLiveUpdatePasses(t *testing.T) {
	env := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ws" || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "не провод", http.StatusBadRequest)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("перехват соединения у поддельного Сёмы: %v", err)
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
		// кадр с изменившимися экранами — как настоящий
		_, _ = conn.Write([]byte(`{"экраны":["health"]}`))
		// и ждём, пока клиент закроет: провод живёт, а не отвечает
		_, _ = io.Copy(io.Discard, conn)
	}), nil)

	req, _ := http.NewRequest("GET", env.edge.URL+"/api/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	// обычный клиент разберёт 101 сам, если не закрывать тело
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("провод не поднялся: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("смены протокола не было: %d %s", resp.StatusCode, raw)
	}
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("кадр по проводу не пришёл: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "health") {
		t.Fatalf("кадр доехал не тот: %q", buf[:n])
	}
}

// Тяжёлый поток не задерживает остальных: у каждого своё соединение.
func TestHeavyStreamDoesNotBlockOthersThroughEdge(t *testing.T) {
	release := make(chan struct{})
	env := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/files/7" {
			// «вложение», которое отдаётся долго. Длину не объявляем: пусть
			// идёт по частям, как настоящий файл, — а объявленная длина,
			// разошедшаяся с числом байтов, уронила бы отдачу молча
			_, _ = w.Write([]byte("start"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-release
			_, _ = w.Write([]byte("end"))
			return
		}
		fmt.Fprint(w, "быстро")
	}), nil)

	going := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.Get(env.edge.URL + "/api/files/7")
		if err != nil {
			t.Errorf("тяжёлый поток: %v", err)
			close(going)
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			t.Errorf("тяжёлый поток не начался: %v", err)
		}
		close(going)
		_, _ = io.ReadAll(resp.Body)
	}()
	<-going

	started := time.Now()
	resp, err := http.Get(env.edge.URL + "/api/ui?screen=health")
	if err != nil {
		t.Fatalf("второй запрос при занятом первом: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	took := time.Since(started)
	close(release)
	<-done
	if !strings.Contains(string(raw), "быстро") {
		t.Fatalf("второй запрос ответил не тем: %q", raw)
	}
	if took > 3*time.Second {
		t.Fatalf("второй запрос ждал первого %s — потоки делят соединение", took.Round(time.Millisecond))
	}
	t.Logf("при занятом тяжёлом потоке экран пришёл за %s", took.Round(time.Millisecond))
}

// Приветствие читается ровно на свою длину: буферизованный читатель утащил
// бы начало потока, и первые байты запроса пропали бы молча.
func TestHelloDoesNotReadAhead(t *testing.T) {
	body, _ := json.Marshal(hello{Kind: KindStream, Stream: "abc123"})
	tail := "GET /api/ui HTTP/1.1\r\nHost: sema\r\n\r\n"
	r := strings.NewReader(fmt.Sprintf("Content-Length: %d\r\n\r\n%s%s", len(body), body, tail))
	kind, stream, err := ReadHello(r)
	if err != nil {
		t.Fatal(err)
	}
	if kind != KindStream || stream != "abc123" {
		t.Fatalf("приветствие разобрано не так: %q %q", kind, stream)
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != tail {
		t.Fatalf("приветствие съело начало потока:\nждали: %q\nосталось: %q", tail, rest)
	}
}

// Кто не Сёма, до приветствия не доходит: сертификат от его центра есть
// только у него.
func TestStrangerFailsHandshake(t *testing.T) {
	env := newHarness(t, echoSema(), nil)
	raw, err := net.DialTimeout("tcp", env.g.cfg.PublicEndpoint, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// Свой корень, не его. Клиентский сертификат чужой при этом **не
	// спрашивает**: иначе рукопожатие разваливалось бы с его стороны, и
	// проверка ничего не говорила бы о нашей — отказать обязан шлюз,
	// потому что серверный сертификат выдан не центром Сёмы.
	foreign := newCerts(t)
	conn := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{foreign.forSema},
		ClientAuth: tls.NoClientCert, MinVersion: tls.VersionTLS13})
	if err := conn.Handshake(); err == nil {
		t.Fatal("чужой прошёл рукопожатие на публичном порту шлюза")
	}
	// и шлюз от этого не умер: следующий запрос браузера проходит
	resp, err := http.Get(env.edge.URL + "/api/ui")
	if err != nil {
		t.Fatalf("после чужого стука шлюз перестал отвечать: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("после чужого стука шлюз отвечает %d", resp.StatusCode)
	}
}

// Тело запроса режется до вноса: занять память у шлюза так же легко,
// как у Сёмы, а он стоит лицом к интернету.
func TestBodyCappedBeforeForwarding(t *testing.T) {
	// пишет обработчик в горутине сервера, читает тест: без atomic гонка
	var arrived atomic.Int64
	arrived.Store(-1)
	env := newHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		arrived.Store(n)
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(200)
	}), func(cfg *Config) { cfg.MaxBody = 64; cfg.MaxFile = 4096 })

	// обычный путь: больше предела не доезжает целиком
	resp, err := http.Post(env.edge.URL+"/api/action", "application/json", strings.NewReader(strings.Repeat("я", 200)))
	if err == nil {
		_ = resp.Body.Close()
	}
	if n := arrived.Load(); n >= 200 {
		t.Fatalf("тело в %d байт доехало целиком при пределе 64", n)
	}
	// а загрузка файла идёт своим, куда большим пределом
	arrived.Store(-1)
	body := strings.Repeat("ф", 500)
	resp2, err := http.Post(env.edge.URL+"/api/files", "application/octet-stream", strings.NewReader(body))
	if err != nil {
		t.Fatalf("загрузка файла не прошла: %v", err)
	}
	_ = resp2.Body.Close()
	if n := arrived.Load(); n != int64(len(body)) {
		t.Fatalf("файл доехал не целиком: %d из %d — предел обычного запроса задел загрузку", n, len(body))
	}
}

// Состояние шлюза говорит, кто держит край и что с сертификатом.
// Пустая графа читалась бы как «всё хорошо», поэтому её не бывает.
func TestStatusNamesEdge(t *testing.T) {
	env := newHarness(t, echoSema(), nil)
	st := env.g.Status()
	if st["край"] != "nginx" {
		t.Fatalf("край назван как %q", st["край"])
	}
	cert, _ := st["сертификат"].(string)
	if cert == "" || !strings.Contains(cert, "срок") {
		t.Fatalf("про сертификат сказано невнятно: %q", cert)
	}
	if st["домен"] != "sema.example.org" {
		t.Fatalf("домен назван как %q", st["домен"])
	}
	if st["сёме"] != env.g.cfg.PublicEndpoint {
		t.Fatalf("публичный адрес назван как %q", st["сёме"])
	}
	// и обработчик отдаёт то же самое: Сёма спрашивает именно им
	h := env.g.Handlers()["шлюз.состояние"]
	if h == nil {
		t.Fatal("Сёме нечем спросить шлюза о себе")
	}
	res, err := h(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Договор двух репозиториев держится на словах ключей, а кодовой
	// зависимости между ними нет: переименуй здесь — и у Сёмы графа молча
	// станет нулём, из которого он сделает вывод «край ничего не отсеял».
	// Поэтому список написан дословно; та же проверка с его стороны —
	// TestСостояниеШлюзаРазобрано в engine.
	answer, _ := res.(map[string]any)
	for _, key := range []string{"домен", "край", "сертификат", "уровень", "браузерам", "сёме",
		"пропущено", "отсеяно", "закрыто", "потоков", "всего", "не внесено"} {
		if _, ok := answer[key]; !ok {
			t.Fatalf("в ответе Сёме нет графы «%s»: %+v", key, answer)
		}
	}
}

// Сквозная проверка потока: байты идут через настоящее соединение от
// поддельного Сёмы, с приветствием и взаимной проверкой сертификатов.
func TestStreamOverRealConnection(t *testing.T) {
	env := newHarness(t, echoSema(), nil)
	conn, err := env.g.stream(context.Background(), "tcp", "сёма:80")
	if err != nil {
		t.Fatalf("поток не открылся: %v", err)
	}
	defer conn.Close()
	req, _ := http.NewRequest("GET", "http://сёма/api/ui?screen=health", nil)
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("ответ по потоку не прочитан: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "/api/ui") {
		t.Fatalf("по потоку приехало не то: %s", raw)
	}
	if env.sema.requests != 1 {
		t.Fatalf("просьб о потоке %d, а поток один", env.sema.requests)
	}
}

// Вход Сёмы на чужие серверы MCP идёт через шлюз: сервер входа скачивает
// документ клиента, браузер человека возвращается с кодом. Без этих путей
// в списке снаружи 404, а у самой Сёмы всё зелёно (30 сентября 2026).
func TestMCPLoginPathsKnown(t *testing.T) {
	for _, p := range []string{"/mcp/oauth/client.json", "/mcp/oauth/callback"} {
		if !Known(p) {
			t.Errorf("%s: шлюз не пускает путь входа MCP", p)
		}
	}
	for _, p := range []string{"/mcp", "/mcp/", "/mcp/oauth/", "/mcp/oauth/другое"} {
		if Known(p) {
			t.Errorf("%s: шлюз пускает лишнее рядом с путями входа", p)
		}
	}
}

// Значок приложения на домашнем экране iPhone: шлюз отсеивал
// apple-touch-icon и icon-180 как «нет такого пути», и значок был пустым
// (1 октября 2026).
func TestAppIconsKnown(t *testing.T) {
	for _, p := range []string{"/icon-180.png", "/icon-192.png", "/icon-512.png", "/icon-maskable-512.png",
		"/icon-maskable.svg", "/apple-touch-icon.png", "/apple-touch-icon-precomposed.png", "/favicon.ico",
		"/icon-badge.png"} {
		if !Known(p) {
			t.Errorf("%s: шлюз не пускает значок приложения", p)
		}
	}
}
