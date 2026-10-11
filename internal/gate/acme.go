package gate

// Свой сертификат на домен — **запасной путь**, а не обычный.
//
// На машине, где шлюз живёт сегодня, край уже держит nginx: 80 и 443
// заняты тремя живыми сайтами, и там же настроен certbot. Забирать у них
// порт ради сертификата, который сосед умеет выписывать сам, — чистый вред:
// первая попытка отняла бы 443, а вторая упёрлась бы в предел выпусков.
//
// Этот путь нужен там, где перед шлюзом нет ничего: отдельная машина,
// поднятая под него одного, или машина без nginx. Условие названо жёстко —
// 80 и 443 должны быть свободны, иначе шлюз отказывается запускаться,
// а не воюет за порт молча.

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// acmeManager — свой публичный сертификат: получение и продление без человека.
type acmeManager struct {
	domain string
	mgr    *autocert.Manager
	cache  string
}

// edgeAddrs — то, без чего выпуск не пройдёт: 443 для TLS-ALPN и 80 для
// HTTP-01. Переменной, а не литералом в теле: проверка занимает адрес сама и
// смотрит, откажется ли шлюз, — а 80 и 443 ей не отдадут без прав
// суперпользователя, и такая проверка сходилась бы по чужой причине. Здесь
// может стоять и полный адрес, а не только порт: net.Listen берёт оба вида.
var edgeAddrs = []string{":80", ":443"}

// OwnCertificate — включить запасной путь: шлюз сам получает и
// продлевает сертификат на домен и сам слушает 443.
//
// Отказ при занятых портах — часть замысла: «не смог выписать» лучше
// узнать при запуске от нас, чем через сутки от браузера, который перестал
// пускать.
func (g *Gateway) OwnCertificate(domain, dir string) error {
	if domain == "" {
		return fmt.Errorf("свой сертификат не выписать без доменного имени")
	}
	for _, port := range edgeAddrs {
		ln, err := net.Listen("tcp", port)
		if err != nil {
			return fmt.Errorf("порт %s занят (%v): свой сертификат выписывать нельзя — "+
				"либо освободите порт, либо оставьте край тому, кто его держит, и слушайте петлю", port, err)
		}
		_ = ln.Close()
	}
	cache := filepath.Join(dir, "acme")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return err
	}
	g.acme = &acmeManager{domain: domain, cache: cache, mgr: &autocert.Manager{
		Cache:      autocert.DirCache(cache),
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(domain),
	}}
	g.cfg.Domain, g.cfg.Edge = domain, "свой ACME"
	g.cfg.BrowserAddr = ":443"
	return nil
}

// Listen — 443 со своим сертификатом плюс 80 для проверки владения именем.
func (a *acmeManager) Listen(ctx context.Context, srv *http.Server) error {
	srv.Addr = ":443"
	srv.TLSConfig = a.mgr.TLSConfig()
	// HTTP-01 и перевод на https: без этого выпуск не проходит вовсе
	verifier := &http.Server{Addr: ":80", Handler: a.mgr.HTTPHandler(nil), ReadHeaderTimeout: 20 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = verifier.Shutdown(sctx)
	}()
	go func() {
		if err := verifier.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("шлюз: порт 80 для проверки имени не поднялся: %v — выпуск сертификата не пройдёт", err)
		}
	}()
	log.Printf("шлюз: принимаю браузеры на 443 со своим сертификатом на %s", a.domain)
	if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Caption — что сказать человеку про сертификат, и насколько это тревожно.
//
// Неудача продления должна быть видна **до** истечения, а не в день, когда
// браузер перестал пускать: за две недели предупреждаем, за трое суток
// тревожим, истёкший называем истёкшим.
func (a *acmeManager) Caption(now time.Time) (text, level string) {
	until, err := a.expiry()
	if err != nil {
		return "своего сертификата ещё нет: " + err.Error(), "warn"
	}
	left := until.Sub(now)
	valid := "свой, годен до " + until.Format("02.01.2006")
	switch {
	case left <= 0:
		return "свой сертификат истёк " + until.Format("02.01.2006") + " — браузеры уже не пускают", "alert"
	case left < 3*24*time.Hour:
		return valid + " — осталось меньше трёх суток, продление не удаётся", "alert"
	case left < 14*24*time.Hour:
		return valid + " — меньше двух недель; обычно продление идёт раньше, значит что-то мешает", "warn"
	}
	return valid, "ok"
}

// expiry — до какого числа годен лежащий в кэше сертификат. Читаем файл, а не
// спрашиваем менеджер: пока никто не обращался, он ничего не знает.
func (a *acmeManager) expiry() (time.Time, error) {
	if a.cache == "" {
		return time.Time{}, fmt.Errorf("кэш сертификатов не задан")
	}
	raw, err := os.ReadFile(filepath.Join(a.cache, a.domain))
	if err != nil {
		return time.Time{}, fmt.Errorf("в кэше его нет (выпуск ещё не проходил)")
	}
	cert, err := firstCert(raw)
	if err != nil {
		return time.Time{}, err
	}
	return cert.NotAfter, nil
}

// firstCert — первый сертификат из PEM-связки: autocert кладёт ключ и
// цепочку в один файл, и нас интересует лист, а не корень.
func firstCert(raw []byte) (*x509.Certificate, error) {
	for len(raw) > 0 {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		return x509.ParseCertificate(block.Bytes)
	}
	return nil, fmt.Errorf("в кэше нет сертификата, только ключ")
}
