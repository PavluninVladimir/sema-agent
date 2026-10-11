package gate

// Свой публичный сертификат — запасной путь, и проверяется здесь то, ради
// чего он вообще заведён: человек должен узнать о неудаче продления **до**
// того дня, когда браузер перестанет пускать. Поэтому проверка смотрит не
// на выпуск (он живёт у Let's Encrypt), а на предупреждение: срок читается
// из кэша, и по нему считается уровень тревоги.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// putInCache — сертификат в том виде, в каком его кладёт autocert: сначала
// закрытый ключ, потом цепочка, всё в одном файле с именем домена. Порядок
// важен: если читать первый блок подряд, а не первый сертификат, наткнёшься
// на ключ и решишь, что сертификата нет вовсе.
func putInCache(t *testing.T, cache, domain string, until time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    until.Add(-90 * 24 * time.Hour),
		NotAfter:     until,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	blob := append(
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	if err := os.WriteFile(filepath.Join(cache, domain), blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Истекающий сертификат виден заранее, а не в день отказа браузера. Тревога
// растёт по мере приближения срока, и в каждой строке названа причина: молча
// показать «ok» за сутки до конца — то же, что не сказать ничего.
func TestExpiringCertVisibleInAdvance(t *testing.T) {
	const domain = "sema.example.org"
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		left  time.Duration
		level string
		words string
	}{
		{"продлится ещё месяц", 30 * 24 * time.Hour, "ok", "годен до"},
		{"меньше двух недель", 10 * 24 * time.Hour, "warn", "меньше двух недель"},
		{"меньше трёх суток", 2 * 24 * time.Hour, "alert", "меньше трёх суток"},
		{"истёк вчера", -24 * time.Hour, "alert", "истёк"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := t.TempDir()
			putInCache(t, cache, domain, now.Add(tc.left))
			a := &acmeManager{domain: domain, cache: cache}
			text, level := a.Caption(now)
			if level != tc.level {
				t.Fatalf("осталось %s: уровень %q, ждали %q (сказано: %s)",
					tc.left, level, tc.level, text)
			}
			if !strings.Contains(text, tc.words) {
				t.Fatalf("осталось %s: в подписи нет «%s»: %s", tc.left, tc.words, text)
			}
		})
	}
}

// Сертификата в кэше нет — так и сказано, с причиной. Пустая графа читалась
// бы как «всё хорошо», а это ровно тот случай, когда наружу никого не пустят.
func TestMissingCertReasonNamed(t *testing.T) {
	a := &acmeManager{domain: "sema.example.org", cache: t.TempDir()}
	text, level := a.Caption(time.Now())
	if level == "ok" {
		t.Fatalf("сертификата нет, а уровень «ok»: %s", text)
	}
	if !strings.Contains(text, "выпуск") {
		t.Fatalf("причина не названа: %s", text)
	}
	// в кэше лежит только ключ (выпуск оборвался на полпути) — это тоже «нет
	// сертификата», а не «сертификат непонятный»
	key := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("не ключ, но блок")})
	if err := os.WriteFile(filepath.Join(a.cache, a.domain), key, 0o600); err != nil {
		t.Fatal(err)
	}
	text, level = a.Caption(time.Now())
	if level == "ok" {
		t.Fatalf("в кэше один ключ, а уровень «ok»: %s", text)
	}
}

// Занятый порт — отказ при запуске, а не тихая война за порт. Иначе первая
// попытка отняла бы 443 у живых сайтов соседа, а вторая упёрлась бы в предел
// выпусков Let's Encrypt — и человек узнал бы об этом от браузера.
func TestOwnCertDoesNotFightForPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	saved := edgeAddrs
	// занимаем ровно тот адрес, который будет пробовать шлюз: на маке
	// подстановочный «:порт» уживается с занятой петлёй, и проверка сходилась
	// бы по чужой причине
	edgeAddrs = []string{ln.Addr().String()}
	t.Cleanup(func() { edgeAddrs = saved })

	g := New(Defaults(), nil)
	err = g.OwnCertificate("sema.example.org", t.TempDir())
	if err == nil {
		t.Fatal("порт занят, а шлюз всё равно взялся выписывать сертификат")
	}
	if !strings.Contains(err.Error(), port) {
		t.Fatalf("в отказе не сказано, какой порт занят: %v", err)
	}
	if g.acme != nil {
		t.Fatal("отказались выписывать, а свой ACME всё равно включён")
	}
	// имени нет — выписывать нечего, и это тоже отказ до запуска
	edgeAddrs = []string{}
	if err := g.OwnCertificate("", t.TempDir()); err == nil {
		t.Fatal("сертификат без доменного имени выписывать нечему")
	}
}

// Порты свободны — путь включается, кэш заводится с правами только
// владельцу, и край в состоянии называется своим. Сам выпуск здесь не идёт:
// он живёт у Let's Encrypt, а проверить его можно только на живой машине.
func TestOwnCertEnablesAndNamesEdge(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	free := ln.Addr().String()
	_ = ln.Close()
	saved := edgeAddrs
	edgeAddrs = []string{free}
	t.Cleanup(func() { edgeAddrs = saved })

	home := t.TempDir()
	g := New(Defaults(), nil)
	if err := g.OwnCertificate("sema.example.org", home); err != nil {
		t.Fatalf("порты свободны, а свой сертификат не включился: %v", err)
	}
	if g.cfg.Edge != "свой ACME" || g.cfg.BrowserAddr != ":443" {
		t.Fatalf("край не переехал на себя: край %q, браузерам %q", g.cfg.Edge, g.cfg.BrowserAddr)
	}
	st, err := os.Stat(filepath.Join(home, "acme"))
	if err != nil {
		t.Fatalf("кэш сертификатов не заведён: %v", err)
	}
	// в кэше лежит закрытый ключ: сосед по машине его читать не должен
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("права на кэш %v, а там закрытый ключ", st.Mode().Perm())
	}
	cert, _ := g.Status()["сертификат"].(string)
	if !strings.Contains(cert, "выпуск") {
		t.Fatalf("состояние молчит про то, что сертификата ещё нет: %q", cert)
	}
}
