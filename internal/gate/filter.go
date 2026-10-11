package gate

// Отсев на краю: список известного, а не угадывание плохого.
//
// Шлюз знает полный список путей Сёмы, и это сильнее любого общего
// движка правил: всё, чего в списке нет, до него не доходит вовсе. Плюс
// пределы частоты (строже на входе), потолок размера тела и временный бан
// после повторных неудачных входов.
//
// Вредоносность по образцам в содержимом мы **не угадываем**: на русском
// тексте с кавычками и кодом ложные срабатывания сломают обычную переписку,
// а пользы почти нет — Сёма не собирает запросов к базе из текста. Поэтому
// сито смотрит на путь, частоту и размер, но никогда в тело.

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// semaPaths — полный список того, что у него есть. Список здесь, а не
// спрашивается у Сёмы, нарочно: если бы шлюз ждал его по проводу, то
// при молчании Сёмы пришлось бы выбирать между «пускать всё» и «не пускать
// ничего», и первое рано или поздно выбрали бы.
//
// Цена — список может разойтись с его маршрутами. Признак расхождения
// честный: новая ручка отвечает «нет такого пути у Сёмы» **и это видно в
// счётчиках шлюза**, а не тихо работает мимо отсева.
var semaPaths = struct {
	exact    []string
	prefixes []string
}{
	exact: []string{
		// оболочка интерфейса: страница, стиль, htmx, работник
		"/", "/index.html", "/app.js", "/htmx.js", "/style.css", "/fonts.css",
		"/sw.js", "/manifest.webmanifest", "/icon.svg",
		// значки приложения: iPhone берёт apple-touch-icon и icon-180 для
		// домашнего экрана, остальные называет manifest — без них значок пустой
		"/icon-180.png", "/icon-192.png", "/icon-512.png", "/icon-maskable-512.png",
		"/icon-maskable.svg", "/apple-touch-icon.png", "/apple-touch-icon-precomposed.png",
		"/favicon.ico",
		// значок пуша в строке состояния Android: без него там серый кружок
		"/icon-badge.png",
		// вход человека и страницы веб-клиента
		"/login", "/ui", "/do", "/upload",
		// ручки, которыми живёт клиент
		"/api/health", "/api/ui", "/api/ws", "/api/action", "/api/files",
		"/api/push/key", "/api/push/subscribe", "/api/push/unsubscribe",
		// вход Сёмы на чужие серверы MCP: сервер входа скачивает документ
		// клиента, а браузер человека возвращается с кодом
		"/mcp/oauth/client.json", "/mcp/oauth/callback",
	},
	prefixes: []string{
		"/fonts/",     // сами файлы шрифтов — часть оболочки
		"/api/auth/",  // церемонии входа
		"/api/files/", // отдача файла по номеру
		"/письмо/",    // тело письма отдельным документом
		"/share/",     // файл по подписанной ссылке: без входа, доступ даёт подпись
	},
}

// NeverAllowed — то, что у Сёмы есть, но интернету там делать нечего.
//
// Вход агента по одноразовому коду — единственная его дверь без сертификата.
// Код живёт минуты и сгорает, но стучаться в эту дверь из интернета незачем:
// агента заводят с машины, а не с чужого адреса.
var NeverAllowed = []string{"/api/agents/enroll"}

// Known — есть ли такой путь у Сёмы.
func Known(p string) bool {
	for _, never := range NeverAllowed {
		if p == never {
			return false
		}
	}
	for _, ok := range semaPaths.exact {
		if p == ok {
			return true
		}
	}
	for _, pre := range semaPaths.prefixes {
		if strings.HasPrefix(p, pre) && len(p) > len(pre) {
			return true
		}
	}
	return false
}

// isLogin — путь входа или церемонии входа: там перебирают, и предел строже.
// Ссылка на файл туда же: её подпись тоже можно пытаться перебирать.
func isLogin(p string) bool {
	return p == "/login" || strings.HasPrefix(p, "/api/auth/") || strings.HasPrefix(p, "/share/")
}

// isUpload — путь, которым загружают вложения: у него свой потолок тела.
func isUpload(p string) bool { return p == "/api/files" || p == "/upload" }

// filterStats — что показать человеку.
type filterStats struct {
	Passed   int
	Filtered int
	Banned   int // адресов под временным баном прямо сейчас
}

// addrCount — что шлюз помнит про один адрес.
type addrCount struct {
	window      time.Time // начало минуты, за которую считаем
	perMinute   int
	loginFails  int // неудачных входов подряд
	bannedUntil time.Time
}

type filter struct {
	cfg   Config
	clock func() time.Time

	mu       sync.Mutex
	addrs    map[string]*addrCount
	passed   int
	filtered int
	swept    time.Time
}

func newFilter(cfg Config, clock func() time.Time) *filter {
	return &filter{cfg: cfg, clock: clock, addrs: map[string]*addrCount{}}
}

func (f *filter) stats() filterStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	banned := 0
	now := f.clock()
	for _, a := range f.addrs {
		if now.Before(a.bannedUntil) {
			banned++
		}
	}
	return filterStats{Passed: f.passed, Filtered: f.filtered, Banned: banned}
}

func (f *filter) countReject() {
	f.mu.Lock()
	f.filtered++
	f.mu.Unlock()
}

// wrap — сито перед вносом к Сёме.
func (f *filter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr := RealAddr(r)
		path := r.URL.Path

		if !Known(path) {
			f.reject(w, r, addr, http.StatusNotFound,
				"такого пути у Сёмы нет — шлюз его не вносит")
			return
		}
		if left, banned := f.isBanned(addr); banned {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(left.Seconds())))
			f.reject(w, r, addr, http.StatusTooManyRequests,
				"адрес закрыт на "+left.Round(time.Second).String()+" после неудачных входов подряд")
			return
		}
		if !f.rateOK(addr, isLogin(path)) {
			f.reject(w, r, addr, http.StatusTooManyRequests, "слишком часто с одного адреса")
			return
		}
		// тело режем до вноса: занять память у шлюза так же легко, как
		// у Сёмы, а он стоит лицом к интернету
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			limit := f.cfg.MaxBody
			if isUpload(path) {
				limit = f.cfg.MaxFile
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}

		f.mu.Lock()
		f.passed++
		f.mu.Unlock()

		// исход входа виден только по ответу Сёмы: перебор ловится здесь
		sw := &codeWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if isLogin(path) && r.Method != http.MethodGet {
			f.loginResult(addr, sw.code)
		}
	})
}

func (f *filter) reject(w http.ResponseWriter, r *http.Request, addr string, code int, why string) {
	f.countReject()
	log.Printf("шлюз: отсеял %s %s от %s — %s", r.Method, r.URL.Path, addr, why)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintln(w, why)
}

// isBanned — закрыт ли адрес и на сколько ещё.
func (f *filter) isBanned(addr string) (time.Duration, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.addrs[addr]
	if a == nil {
		return 0, false
	}
	if left := a.bannedUntil.Sub(f.clock()); left > 0 {
		return left, true
	}
	return 0, false
}

// rateOK — не слишком ли часто с этого адреса за последнюю минуту.
func (f *filter) rateOK(addr string, login bool) bool {
	limit := f.cfg.RateNormal
	if login {
		limit = f.cfg.RateLogin
	}
	if limit <= 0 {
		return true
	}
	now := f.clock()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweep(now)
	a := f.addrs[addr]
	if a == nil {
		a = &addrCount{window: now}
		f.addrs[addr] = a
	}
	if now.Sub(a.window) >= time.Minute {
		a.window, a.perMinute = now, 0
	}
	a.perMinute++
	return a.perMinute <= limit
}

// loginResult — вход удался или нет. Подряд неудачные закрывают адрес: так
// перебор упирается в край, а не в Сёму.
func (f *filter) loginResult(addr string, code int) {
	now := f.clock()
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.addrs[addr]
	if a == nil {
		a = &addrCount{window: now}
		f.addrs[addr] = a
	}
	if code < 400 {
		a.loginFails = 0 // вошёл — счёт неудач обнуляется
		return
	}
	a.loginFails++
	if f.cfg.LoginTries > 0 && a.loginFails >= f.cfg.LoginTries {
		a.bannedUntil = now.Add(f.cfg.LoginBan)
		a.loginFails = 0
		log.Printf("шлюз: закрыл %s на %s — %d неудачных входов подряд",
			addr, f.cfg.LoginBan, f.cfg.LoginTries)
	}
}

// sweep — забыть адреса, о которых давно ничего не слышно. Без этого
// карта растёт от каждого сканера в интернете, а их там много.
func (f *filter) sweep(now time.Time) {
	if now.Sub(f.swept) < 5*time.Minute {
		return
	}
	f.swept = now
	for addr, a := range f.addrs {
		if now.Sub(a.window) > time.Hour && now.After(a.bannedUntil) {
			delete(f.addrs, addr)
		}
	}
}

// codeWriter — код, с которым ответила Сёма: по нему видно, удался ли вход.
type codeWriter struct {
	http.ResponseWriter
	code int
}

func (w *codeWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *codeWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap — обёртка не должна скрывать от библиотеки то, что умеет
// настоящий ответ: без этого перехват соединения под провод живого
// обновления отвечал бы «не умею», и /api/ws не работал бы вовсе.
func (w *codeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
