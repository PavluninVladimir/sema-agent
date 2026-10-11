package gate

// Сторона шлюза в шифрованном разговоре с Сёмой.
//
// Сокет открывает Сёма, но клиент TLS по-прежнему шлюз: роли
// сертификатов от
// направления TCP не зависят. Так нетронутыми остаются выдача сертификатов,
// вход по одноразовому коду, разрешение способностей поимённо и тревога на
// смену отпечатка — весь этот пласт переделывать не пришлось.

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"time"
)

// HandshakeTimeout — сколько ждём, пока на том конце окажется Сёма. На
// публичный порт стучатся все кому не лень, и висеть на каждом стуке
// нельзя.
const HandshakeTimeout = 10 * time.Second

// tlsClient — готовая настройка: чем себя доказываем и по чему узнаём Сёму.
type tlsClient struct{ conf *tls.Config }

// TLS — настройка для приёма звонков Сёмы. Берётся у провода: удостоверение
// одно на весь агент, и второй его копии быть не должно.
func (g *Gateway) TLS(conf *tls.Config) { g.tls = &tlsClient{conf: conf} }

func (g *Gateway) tlsForSema() (*tlsClient, error) {
	if g.tls == nil || g.tls.conf == nil {
		return nil, fmt.Errorf("шлюзу не дано удостоверение: получите его по приглашению (-enroll <код> -role шлюз)")
	}
	return g.tls, nil
}

// wrap — стать клиентом TLS на уже принятом сокете.
func (c *tlsClient) wrap(ctx context.Context, raw net.Conn) (net.Conn, error) {
	conn := tls.Client(raw, c.conf)
	hctx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		return nil, err
	}
	return conn, nil
}

// newStreamToken — номер потока: по нему шлюз узнаёт своё соединение среди
// пришедших. Угадать его нечего — попасть в приём потоков можно только
// пройдя взаимную проверку сертификатов; случайность здесь от путаницы, а
// не от врага.
func newStreamToken() string {
	raw := make([]byte, 9)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw)
}
