package main

// Местная дверь демона: сокет в домашней папке агента вместо порта с
// токеном. Через неё ходят обёртки агентов разработки и хуки памяти.
//
// Почему сокет, а не порт: токен в конфигурации — это тот самый секрет,
// ради избавления от которого затевался переезд памяти на провод. Права
// на файл сокета (0600) решают ту же задачу без строки, которую можно
// подсмотреть, скопировать в чужую папку или забыть отозвать.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// socketPath — где живёт местная дверь.
func socketPath(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, "agent.sock")
}

// listenLocal — открыть дверь. Занятый сокет — не ошибка сборки, а
// сигнал «демон уже есть»: вызывающий станет обёрткой.
func listenLocal(path string) (net.Listener, error) {
	if path == "" {
		return nil, errors.New("не задана домашняя папка агента")
	}
	// Замок берём до всего остального: два процесса, одновременно
	// увидевшие мёртвый файл сокета, оба снимали его и слушали — второй
	// сносил дверь первого, и демон оставался без двери. Замок держится
	// дескриптором всю жизнь процесса, убитый отпускает его сам.
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, syscall.EADDRINUSE
	}
	// замок наш — значит и мёртвый файл сокета наш: живого демона нет
	if _, err := os.Stat(path); err == nil {
		_ = os.Remove(path)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		lock.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	heldLock = lock // до конца жизни процесса
	return l, nil
}

// heldLock — дескриптор замка местной двери. Живёт до конца процесса:
// закрыть его значит пустить второго демона.
var heldLock *os.File

// serveLocal — тот же JSON-RPC построчно, что и по stdio: один клиент —
// одно соединение, разговор идёт до его закрытия.
func (s *server) serveLocal(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			r := bufio.NewReaderSize(c, 1<<20)
			enc := json.NewEncoder(c)
			sess := &session{}
			for {
				line, err := r.ReadBytes('\n')
				if len(strings.TrimSpace(string(line))) > 0 {
					var req request
					if json.Unmarshal(line, &req) == nil {
						if resp := s.handle(req, sess); resp != nil {
							if enc.Encode(resp) != nil {
								return
							}
						}
					}
				}
				if err != nil {
					return
				}
			}
		}(conn)
	}
}

// localClient — разговор с уже запущенным демоном.
type localClient struct {
	conn net.Conn
	r    *bufio.Reader
	enc  *json.Encoder
}

// localWait — сколько ждём местную дверь. Демон свой и рядом, поэтому
// срок короткий: если он не отвечает секунды, он не ответит и минуты, а
// хук всё это время держит сессию агента разработки.
const localWait = 5 * time.Second

func dialLocal(path string) (*localClient, error) {
	if path == "" {
		return nil, errors.New("не задана домашняя папка агента")
	}
	c, err := net.DialTimeout("unix", path, localWait)
	if err != nil {
		return nil, err
	}
	return &localClient{conn: c, r: bufio.NewReaderSize(c, 1<<20), enc: json.NewEncoder(c)}, nil
}

func (c *localClient) Close() { _ = c.conn.Close() }

// ask — один запрос, один ответ. Уведомления (кадры без id) демон не
// присылает, поэтому очередь ответов не нужна.
func (c *localClient) ask(req any) (*response, error) {
	// ответ памяти идёт по проводу к Сёме, поэтому срок здесь щедрее
	// самого разговора с ней, но конечный
	_ = c.conn.SetDeadline(time.Now().Add(3 * time.Minute))
	defer c.conn.SetDeadline(time.Time{})
	if err := c.enc.Encode(req); err != nil {
		return nil, err
	}
	line, err := c.r.ReadBytes('\n')
	if len(strings.TrimSpace(string(line))) == 0 {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("демон агента ответил пусто")
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// localAlive — есть ли живой демон. Нужно при старте: своего пула
// языковых серверов поднимать не надо, если он уже поднят.
func localAlive(path string) bool {
	c, err := dialLocal(path)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func logLocal(path string) {
	log.Printf("местная дверь агента: %s", path)
}

// sendRaw — переслать кадр как есть. Обёртка не разбирает разговор: чем
// меньше она понимает, тем меньше ломает.
func (c *localClient) sendRaw(line []byte) error {
	_, err := c.conn.Write(append(bytes.TrimSpace(line), '\n'))
	return err
}

// readRaw — ответ строкой. Спрашивать его можно только на кадр с
// номером: на уведомление демон молчит, и ожидание повисло бы навсегда.
func (c *localClient) readRaw() ([]byte, error) {
	out, err := c.r.ReadBytes('\n')
	if len(bytes.TrimSpace(out)) == 0 && err != nil {
		return nil, err
	}
	return out, nil
}
