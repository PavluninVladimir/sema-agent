// Сёма-агент. У экземпляра одна роль — житель машины или шлюз
// (role.go), они не совмещаются: житель читает репозитории человека,
// шлюз смотрит в интернет, и в одном теле это разные полномочия.
//
// Житель машины: карточки кода (пул языковых серверов вместо содержимого
// файлов), память Сёмы и уведомления. Claude Code и Codex говорят с ним по
// stdio и по местной двери (сокет), Сёма — по своему постоянному проводу
// (internal/link), не по HTTP: тот остался только для одноразового входа по
// приглашению. Житель только читает: файлов не правит, команд не запускает.
//
// Шлюз (internal/gate): лицо Сёмы в интернете. Держит домен, принимает
// запросы браузеров и вносит их к нему тем же проводом. Направление TCP
// здесь обратное — **звонит Сёма**, потому что прячется он, а у шлюза
// адрес постоянный; роли шифрованного разговора при этом прежние. Поэтому у
// шлюза нет `-link`: набирать некуда, он слушает.
//
// Запуск:
//
//	semaagent                                   # для агента разработки: сам найдёт демона
//	semaagent -enroll <код> -role житель         # разово: получить удостоверение
//	semaagent -link sema.local:8766              # житель-демон: провод к Сёме и местная дверь
//	semaagent -gate-sema 0.0.0.0:8767 \          # шлюз: ждёт звонка Сёмы на один порт
//	          -gate-http 127.0.0.1:8770 \        #   и принимает браузеры из петли, за nginx
//	          -gate-domain sema.example.org
//
// Ставится и обновляется одинаково на Linux и на Mac — порядок и файлы
// службы (systemd, launchd) лежат в deploy/, см. deploy/README.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/PavluninVladimir/sema-agent/internal/desk"
	"github.com/PavluninVladimir/sema-agent/internal/gate"
	semalink "github.com/PavluninVladimir/sema-agent/internal/link"
	"github.com/PavluninVladimir/sema-agent/internal/lsp"
)

// candidateCapabilities — что машина вообще умеет, без учёта роли.
// Показывать уведомления умеет не всякая машина, поэтому способность
// объявляется только там, где ей есть чем заняться. Роль сужает этот
// список дальше (capabilitiesFor) — объявляет агент, разрешает человек, а
// роль решает раньше обоих.
// Развилка вынесена в отдельную функцию нарочно: на маке desk.Available()
// всегда true, и тест, спрашивающий у машины, на маке проверял бы одну
// половину правила, а вторую — только на Linux. Так проверяются обе
// стороны где угодно.
func candidateCapabilities() []string { return machineCapabilities(desk.Available()) }

func machineCapabilities(hasDesktop bool) []string {
	// шлюзовая есть на любой машине: ей не нужен ни рабочий стол, ни
	// языковые серверы. Роли она всё равно не перепадёт — capabilitiesFor
	// отсеет её у жителя с названной причиной
	can := []string{"код", "память", CanGateway}
	if hasDesktop {
		can = append(can, "уведомления")
	}
	return can
}

// reportDesktop — значок у часов, помощник на Swift и osascript
// есть только на маке: это Cocoa. На Linux их нет, и агент говорит об этом
// вслух один раз при старте, а не падает и не молчит — обещать Сёме
// доставку, которой не будет, хуже, чем её не обещать. Тегов сборки для
// этого не нужно: код собирается под обе системы, хватает проверки во
// время работы (design.md, решение 2).
func reportDesktop() {
	if desk.Available() {
		return
	}
	log.Printf("рабочего стола на %s нет: ни значка у часов, ни системных уведомлений — "+
		"способность «уведомления» Сёме не объявляю", runtime.GOOS)
}

// capabilitiesFor — candidateCapabilities, отфильтрованные ролью.
// Отказанное не пропадает молча: причина идёт в журнал, а не только в
// код (spec: «попытка объявить способности чужой роли — событие с
// причиной, а не тихий пропуск»).
func capabilitiesFor(role Role) []string {
	allowed, denied := allowedForRole(role, candidateCapabilities())
	for name, why := range denied {
		log.Printf("способность %q не объявлена Сёме: %s", name, why)
	}
	return allowed
}

// Version — версия агента: печатается Сёме при знакомстве. Обновление не
// должно быть невидимым (design.md, риск «Обновление меняет поведение
// молча»). Настоящее значение подставляет линковщик при сборке
// (-ldflags "-X main.Version=…", см. Makefile); без него — «dev».
var Version = "dev"

// defaultHome — где лежит удостоверение агента.
func defaultHome() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".sema-agent")
	}
	return ".sema-agent"
}

// agentFlags — все флаги в одном наборе. Отдельной функцией, а не прямо в
// main: файлы службы в deploy/ передают флаги строкой, и перед ними эта
// функция — единственный справочник правды. Тест сверяет одно с другим:
// переименованный флаг иначе сломал бы службу молча, и узналось бы это
// только после перезагрузки машины.
type agentFlags struct {
	enroll, role, sema, link, home, hook, who *string
	maxServers, idleMin                       *int
	knowledgeMs                               *int
	// шлюз: два адреса, и путать их нельзя. gateSema — публичный порт,
	// куда звонит Сёма (единственное, что открывают на домашнем роутере);
	// gateHTTP — петля, откуда берёт запросы край (nginx).
	gateSema, gateHTTP, gateDomain *string
	gateACME                       *bool
}

func newFlags(fs *flag.FlagSet) *agentFlags {
	return &agentFlags{
		enroll:      fs.String("enroll", "", "одноразовый код приглашения от Сёмы: получить удостоверение и выйти"),
		role:        fs.String("role", string(RoleResident), "роль, которую агент просит себе при входе по приглашению: житель или шлюз"),
		sema:        fs.String("sema", "http://127.0.0.1:8765", "адрес Сёмы для входа по приглашению"),
		link:        fs.String("link", "", "адрес Сёмы для постоянной связи, например 127.0.0.1:8766"),
		home:        fs.String("home", defaultHome(), "где хранить удостоверение агента"),
		hook:        fs.String("hook", "", "вид события хука: session-start, note, session-end, subagent-note"),
		who:         fs.String("agent", "Claude Code", "кто спрашивает память: Claude Code, Codex"),
		maxServers:  fs.Int("max", 3, "сколько языковых серверов держать одновременно"),
		idleMin:     fs.Int("idle", 20, "через сколько минут тишины гасить языковой сервер"),
		knowledgeMs: fs.Int("knowledge-ms", 800, "сколько миллисекунд карточка ждёт приложение из памяти Сёмы; истёк срок — карточка уходит с пометкой"),
		gateSema: fs.String("gate-sema", "", "шлюз: публичный адрес, куда звонит Сёма, например 0.0.0.0:8767. "+
			"Один порт на всё — и провод, и потоки браузера"),
		gateHTTP: fs.String("gate-http", gate.Defaults().BrowserAddr,
			"шлюз: где принимать браузеры; по умолчанию петля, край держит nginx"),
		gateDomain: fs.String("gate-domain", "", "шлюз: имя, под которым Сёма видна снаружи"),
		gateACME: fs.Bool("gate-acme", false, "шлюз: получать публичный сертификат самому (запасной путь; "+
			"нужны свободные 80 и 443 — там, где край держит nginx, включать не надо)"),
	}
}

func main() {
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	f := newFlags(fs)
	_ = fs.Parse(os.Args[1:])
	enroll, roleFlag, sema, link := f.enroll, f.role, f.sema, f.link
	home, hook, who := f.home, f.hook, f.who
	maxServers, idleMin := f.maxServers, f.idleMin

	log.SetOutput(os.Stderr) // stdout занят разговором по stdio

	// хук памяти: событие приходит на стандартный вход, ответ уходит на
	// стандартный выход. Код возврата всегда нулевой — сессия агента
	// разработки не должна падать из-за памяти
	if *hook != "" {
		printHook(*hook, *who, runHook(*hook, *who, *home, os.Stdin))
		return
	}

	// вход по приглашению: получаем удостоверение и выходим — дальше
	// агент запускается уже с ним. Роль запрошена флагом, но главное
	// слово за Сёмой: код выдавала она, и если человек выбрал роль там,
	// её ответ это подтвердит или поправит (Enroll разбирает оба случая).
	if *enroll != "" {
		role, ok := ParseRole(*roleFlag)
		if !ok {
			log.Fatalf("роль %q не распознана: житель или шлюз", *roleFlag)
		}
		id, err := semalink.Enroll(*sema, *enroll, string(role), capabilitiesFor(role))
		if err != nil {
			log.Fatalf("вход по приглашению: %v", err)
		}
		if err := semalink.Save(*home, id); err != nil {
			log.Fatalf("удостоверение не сохранилось: %v", err)
		}
		if id.Role != string(role) {
			// человек выбрал роль там, где выдавал код, — и его слово
			// главнее нашего флага. Молча подменять роль нельзя: с ней
			// меняется всё, что агент вообще станет объявлять
			log.Printf("Сёма закрепил роль «%s», а просили «%s»: способности буду объявлять по его роли", id.Role, role)
		}
		log.Printf("удостоверение получено: агент «%s», роль «%s», хранится в %s", id.Name, id.Role, *home)
		return
	}

	// демон живёт сокетом в домашней папке: его слышат и обёртки, и хуки.
	// Роль процесса решает не флаг, а дверь: кто её открыл — тот демон,
	// остальные к нему приходят
	sock := socketPath(*home)
	// демон — тот, кто держит провод к Сёме: он же открывает местную
	// дверь остальным. У шлюза провод тот же, но звонит по нему Сёма,
	// поэтому флага -link у него нет вовсе — демоном его делает -gate-sema
	daemon := *link != "" || *f.gateSema != ""
	var door net.Listener
	if daemon {
		l, err := listenLocal(sock)
		if err != nil {
			log.Fatalf("местная дверь %s занята: демон уже запущен (%v)", sock, err)
		}
		door = l
		defer door.Close()
	} else if localAlive(sock) {
		// демон уже запущен — работаем тонкой обёрткой: языковой сервер
		// поднимается один раз на всех клиентов, а не по разу на каждого
		proxyLocal(sock)
		return
	}

	id, hasIdentity := semalink.Load(*home)
	role := roleFromIdentity(id.Role, hasIdentity)
	reportDesktop()
	caps := capabilitiesFor(role)

	var pool *lsp.Pool
	if hasCap(caps, "код") {
		pool = lsp.NewPool(lsp.DefaultRegistry(), *maxServers, time.Duration(*idleMin)*time.Minute)
		defer pool.Close()
	}

	srv := &server{}
	srv.pool = pool
	srv.loadSavings(savingsPath(*home))
	srv.knowledgeWait = time.Duration(*f.knowledgeMs) * time.Millisecond
	if pool != nil {
		srv.tools = tools(pool, srv)
	}

	// демон живёт сетью: разговор по stdio ведём заодно, но выход из него
	// (а он случается сразу, если агента запустили в фоне) не должен
	// ронять демона
	// постоянная связь с Сёмой: звоним сами и держим соединение
	var toSema memoryCaller
	var conn *semalink.Conn
	// шлюз держит тот же провод, но соединение по нему открывает Сёма:
	// прячется он, а звонит тот, кого не найти по адресу
	asGateway := hasCap(caps, CanGateway) && *f.gateSema != ""
	if *f.gateSema != "" && !hasCap(caps, CanGateway) {
		log.Fatalf("шлюзовая этой роли не положена: роль «%s». "+
			"Шлюза заводят своим приглашением — роль выбирается там, где Сёма выдаёт код", role)
	}
	if *link != "" || asGateway {
		if !hasIdentity {
			log.Fatalf("нет удостоверения в %s: получите его по приглашению (-enroll <код>)", *home)
		}
		// обработчики способности «код» идут по проводу, только когда роль
		// её вообще допускает — иначе Сёма получил бы список методов,
		// которых у этой роли по спеке нет, даже если бы попросила
		handlers := map[string]semalink.Handler{}
		if hasCap(caps, "код") {
			handlers = agentHandlers(srv)
		}
		conn = semalink.New(id, *link, caps, string(role), Version, handlers)
		toSema = conn
		srv.sema = conn
		// рабочий стол: значок у часов, уведомления Сёмы и добор
		// пропущенного при подключении — только у роли, которой это положено
		if hasCap(caps, "уведомления") {
			n := newNotifier(*home, conn)
			for name, h := range n.handlers() {
				conn.Handle(name, h)
			}
			conn.OnState = n.linkState
			n.Start()
		}
	}
	// инструменты памяти появляются последними: обработчики способности
	// «код» собраны выше, и память в них попасть не должна — иначе Сёма
	// спрашивала бы саму себя через агента. Реестр дописываем до того,
	// как провод и двери начнут его читать
	if hasCap(caps, "память") {
		srv.tools = append(srv.tools, memoryTools(toSema)...)
	}
	// Звонит либо агент, либо Сёма — но не оба. Житель машины набирает сам
	// (ноутбук спит, меняет сеть, стоит за роутером), а к шлюзу звонит
	// Сёма: адрес постоянный у шлюза, а прячется Сёма.
	if conn != nil && *link != "" {
		go conn.Run(context.Background())
	}
	if asGateway {
		startGateway(context.Background(), f, conn, *home)
	}

	if door != nil {
		logLocal(sock)
		go srv.serveLocal(door)
	}

	if daemon {
		// демон без сетевого рта живёт сокетом; стандартный вход под
		// launchd это /dev/null, и выход из разговора по stdio не повод
		// гасить дверь
		go srv.serveStdio(os.Stdin, os.Stdout)
		select {}
	}
	srv.serveStdio(os.Stdin, os.Stdout)
}

// repoRoot — корень проекта для файла: языковой сервер работает с
// проектом целиком, а не с папкой. Ищем вверх по приметам сборки, потом
// git; папка файла — последнее средство.
var rootMarks = []string{"go.mod", "Cargo.toml", "package.json", "Package.swift", "pyproject.toml", ".git"}

// findFile — путь к файлу репозитория по корню работы и пути из записи
// памяти. Координата хранится по-разному: у старых записей это путь от
// корня репозитория («internal/engine/rag.go»), у новых — от корня
// зонтика («server/internal/web/icons.go»). Пробуем оба, а гадать по
// всей машине не пытаемся: не нашли — так и говорим.
func findFile(roots []string, file string) (string, bool) {
	file = strings.TrimPrefix(strings.TrimSpace(file), "/")
	if file == "" {
		return "", false
	}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		candidates := []string{filepath.Join(root, file)}
		for _, sub := range []string{"server", "app", "agent"} {
			candidates = append(candidates, filepath.Join(root, sub, file))
		}
		for _, cand := range candidates {
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				return cand, true
			}
		}
	}
	return "", false
}

// findDir — папка по рабочим корням, как findFile для файла.
func findDir(roots []string, dir string) (string, bool) {
	dir = strings.Trim(strings.TrimSpace(dir), "/")
	if dir == "" {
		return "", false
	}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		candidates := []string{filepath.Join(root, dir)}
		for _, sub := range []string{"server", "app", "agent", "server/internal"} {
			candidates = append(candidates, filepath.Join(root, sub, dir))
		}
		for _, cand := range candidates {
			if st, err := os.Stat(cand); err == nil && st.IsDir() {
				return cand, true
			}
		}
	}
	return "", false
}

func repoRoot(path string) string {
	dir := path
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		dir = filepath.Dir(path)
	}
	for cur := dir; cur != "/" && cur != "."; cur = filepath.Dir(cur) {
		for _, mark := range rootMarks {
			if _, err := os.Stat(filepath.Join(cur, mark)); err == nil {
				return cur
			}
		}
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			return root
		}
	}
	return dir
}

// tools — способность «код»: шесть вопросов, на которые агент отвечает
// карточками вместо содержимого файлов.
func tools(pool *lsp.Pool, srv *server) []tool {
	card := func(c lsp.Card, err error) (string, error) {
		if err != nil && c.Text == "" {
			return "", err
		}
		srv.note(c.Lines, c.Saved, c.WithKnowledge)
		head := "[ярус " + c.Tier + "]"
		if c.Note != "" {
			head += " " + c.Note
		}
		return head + "\n" + c.Text, nil
	}
	ctx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 90*time.Second)
	}
	// known — карточка с приложением из памяти Сёмы: что здесь уже
	// ломалось и каким принципом держится пакет. Одна дверь, один вызов
	known := func(a map[string]any, c lsp.Card, err error) (string, error) {
		if err == nil || c.Text != "" {
			srv.attachKnowledge(&c, str(a, "path"), str(a, "client"))
		}
		return card(c, err)
	}
	return []tool{
		{
			Name:   "symbols",
			Desc:   "Оглавление файла вместо его текста: что объявлено, какого вида и на какой строке, плюс что про это место известно памяти Сёмы. Аргумент path — путь к файлу.",
			Schema: schema(map[string]string{"path": "путь к файлу"}, "path"),
			Run: func(a map[string]any) (string, error) {
				p := str(a, "path")
				c, cancel := ctx()
				defer cancel()
				card, err := pool.Symbols(c, repoRoot(p), p)
				return known(a, card, err)
			},
		},
		{
			Name:   "context",
			Desc:   "Карточка символа: что это, где объявлен, краткое описание, где его зовут и что про него известно памяти Сёмы. Аргументы path и name.",
			Schema: schema(map[string]string{"path": "путь к файлу", "name": "имя символа"}, "path", "name"),
			Run: func(a map[string]any) (string, error) {
				p := str(a, "path")
				c, cancel := ctx()
				defer cancel()
				card, err := pool.Context(c, repoRoot(p), p, str(a, "name"))
				return known(a, card, err)
			},
		},
		{
			Name:   "impact",
			Desc:   "Радиус поражения перед правкой: сколько мест сломается и какие тесты это трогают. Аргументы path и name.",
			Schema: schema(map[string]string{"path": "путь к файлу", "name": "имя символа"}, "path", "name"),
			Run: func(a map[string]any) (string, error) {
				p := str(a, "path")
				c, cancel := ctx()
				defer cancel()
				return card(pool.Impact(c, repoRoot(p), p, str(a, "name")))
			},
		},
		{
			Name: "definition",
			Desc: "Где объявлено имя в этом месте файла. Аргументы path, line (с единицы) и column.",
			Schema: map[string]any{"type": "object", "properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "путь к файлу"},
				"line":   map[string]any{"type": "number", "description": "строка, считая с единицы"},
				"column": map[string]any{"type": "number", "description": "столбец, считая с нуля"},
			}, "required": []string{"path", "line"}},
			Run: func(a map[string]any) (string, error) {
				p := str(a, "path")
				c, cancel := ctx()
				defer cancel()
				return card(pool.Definition(c, repoRoot(p), p, num(a, "line", 1), num(a, "column", 0)))
			},
		},
		{
			Name: "diagnostics",
			Desc: "Что сломано сейчас, без запуска сборки. Аргумент path; можно передать text — тогда проверяется он, а файл на диске не нужен.",
			Schema: schema(map[string]string{
				"path": "путь к файлу (или условный адрес, если передан text)",
				"text": "содержимое для проверки; пусто — читаем файл",
			}, "path"),
			Run: func(a map[string]any) (string, error) {
				p := str(a, "path")
				c, cancel := ctx()
				defer cancel()
				return card(pool.Diagnostics(c, repoRoot(p), p, str(a, "text")))
			},
		},
		{
			Name:   "search",
			Desc:   "Поиск символа по проекту: отдаёт места, а не содержимое. Аргументы query и path (любой файл проекта, чтобы понять, о каком репозитории речь).",
			Schema: schema(map[string]string{"query": "что ищем", "path": "любой файл проекта"}, "query", "path"),
			Run: func(a map[string]any) (string, error) {
				p := str(a, "path")
				c, cancel := ctx()
				defer cancel()
				return card(pool.Search(c, repoRoot(p), p, str(a, "query")))
			},
		},
		{
			Name:   "agent_status",
			Desc:   "Что умеет агент прямо сейчас: какие языковые серверы установлены, какие подняты, сколько строк не прочитано благодаря карточкам.",
			Schema: map[string]any{"type": "object", "properties": map[string]any{}},
			Run: func(map[string]any) (string, error) {
				var b strings.Builder
				b.WriteString("Языки:\n")
				for _, l := range pool.Registry().Langs() {
					mark := "нет сервера"
					if l.Available() {
						mark = "готов"
					}
					fmt.Fprintf(&b, "  %-12s %-28s %s\n", l.Name, strings.Join(l.Command, " "), mark)
				}
				alive := pool.Alive()
				fmt.Fprintf(&b, "\nПодняты сейчас: %d\n", len(alive))
				for _, a := range alive {
					b.WriteString("  " + a + "\n")
				}
				s := srv.savedNow()
				fmt.Fprintf(&b, "\nОтветов %d · строк в карточках %d · строк не прочитано %d\n",
					s.Calls, s.Cards, s.Lines)
				return b.String(), nil
			},
		},
	}
}

// agentHandlers — способность «код» с той стороны провода: Сёма зовёт то
// же, что и агенты разработки, только по своему соединению.
func agentHandlers(srv *server) map[string]semalink.Handler {
	call := func(name string) semalink.Handler {
		return func(ctx context.Context, params json.RawMessage) (any, error) {
			var args map[string]any
			if len(params) > 0 {
				if err := json.Unmarshal(params, &args); err != nil {
					return nil, fmt.Errorf("аргументы не разобраны: %w", err)
				}
			}
			t := srv.byName(name)
			if t == nil {
				return nil, fmt.Errorf("агент не умеет %q", name)
			}
			text, err := t.Run(args)
			if err != nil {
				return nil, err
			}
			return map[string]any{"text": text}, nil
		}
	}
	out := map[string]semalink.Handler{}
	for _, t := range srv.tools {
		out["код."+t.Name] = call(t.Name)
	}
	// «Умения» — тот же список, что видит Codex по MCP: Сёма спрашивает
	// его по каналу, а не по HTTP, и держит справочник свежим.
	// «Экономия» — счёт непрочитанного в цифрах, чтобы Сёма показывал
	// пользу строкой, а не верой на слово.
	out["код.экономия"] = func(ctx context.Context, _ json.RawMessage) (any, error) {
		s := srv.savedNow()
		ct, gr := srv.pool.ToolsReady()
		// «за неделю» — отдельно: по нарастающему итогу не видно, стали
		// ли карточки спрашивать после подсказки в ответе хука
		return map[string]any{"calls": s.Calls, "card_lines": s.Cards, "saved_lines": s.Lines,
			"with_knowledge": s.WithKnowledge, "week": s.savedWeek(),
			"ctags": ct, "grep": gr}, nil
	}
	// «Место» — жив ли символ, на который показывает запись памяти Сёмы.
	// Файлы репозитория видит только эта машина, поэтому спрашивать может
	// лишь житель. Ответ «не знаю» (знаем: false) честнее выдумки: по нему
	// Сёма координату не трогает.
	out["код.место"] = func(ctx context.Context, params json.RawMessage) (any, error) {
		var a struct {
			Roots  []string `json:"корни"`
			Root   string   `json:"корень"`
			File   string   `json:"файл"`
			Symbol string   `json:"символ"`
			Dir    string   `json:"папка"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &a); err != nil {
				return nil, fmt.Errorf("аргументы не разобраны: %w", err)
			}
		}
		if a.Root != "" {
			a.Roots = append(a.Roots, a.Root)
		}
		if a.Dir != "" && a.File == "" {
			// привязка к папке: есть ли ещё такая папка в рабочих корнях
			dir, ok := findDir(a.Roots, a.Dir)
			return map[string]any{"папка": ok, "есть": ok, "знаем": len(a.Roots) > 0, "ярус": "папка", "путь": dir}, nil
		}
		path, ok := findFile(a.Roots, a.File)
		if !ok {
			// файла нет ни в одной рабочей папке — ответ определённый
			// ровно настолько, насколько полон список папок
			return map[string]any{"файл": false, "есть": false, "знаем": len(a.Roots) > 0, "ярус": "файл"}, nil
		}
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		found, known, tier := srv.pool.HasSymbol(c, repoRoot(path), path, a.Symbol)
		return map[string]any{"файл": true, "есть": found, "знаем": known, "ярус": tier, "путь": path}, nil
	}
	// список замораживаем здесь: дальше в реестр добавятся инструменты
	// памяти, а Сёма зовёт агента только по способности «код» — назвать
	// ему то, чего он позвать не может, значит соврать
	names := make([]string, 0, len(srv.tools))
	for _, t := range srv.tools {
		names = append(names, t.Name)
	}
	out["код.умения"] = func(ctx context.Context, _ json.RawMessage) (any, error) {
		return map[string]any{"tools": names}, nil
	}
	return out
}
