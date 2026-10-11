package gate

// Удостоверения для проверок: свой корень, серверный сертификат Сёмы и
// клиентский шлюза. Настоящие, а не поддельные: весь смысл провода в
// том, что стороны узнают друг друга по сертификату, и проверять это в
// обход TLS значило бы проверять намерение, а не поведение.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

type testCerts struct {
	root     *x509.Certificate
	rootKey  ed25519.PrivateKey
	rootPool *x509.CertPool
	forSema  tls.Certificate // серверный, имя «sema»
	forEdge  tls.Certificate // клиентский, имя шлюза
}

func newCerts(t *testing.T) *testCerts {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Сёма"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	certs := &testCerts{root: root, rootKey: priv, rootPool: pool}
	certs.forSema = certs.issue(t, "sema", []string{"sema"}, x509.ExtKeyUsageServerAuth)
	certs.forEdge = certs.issue(t, "край", nil, x509.ExtKeyUsageClientAuth)
	return certs
}

func (certs *testCerts) issue(t *testing.T, name string, dns []string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		DNSNames: dns, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, certs.root, pub, certs.rootKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}
}

// edgeTLS — настройка шлюза: он клиент TLS, кто бы ни открыл сокет.
func (certs *testCerts) edgeTLS() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{certs.forEdge}, RootCAs: certs.rootPool,
		ServerName: "sema", MinVersion: tls.VersionTLS13}
}

// semaTLS — настройка Сёмы: он сервер TLS и требует сертификат от своего центра.
func (certs *testCerts) semaTLS() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{certs.forSema},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: certs.rootPool,
		MinVersion: tls.VersionTLS13}
}
