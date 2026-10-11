package main

// Шлюз со стороны запуска: собрать край, дать ему удостоверение и
// начать принимать. Само устройство — в internal/gate; здесь только сборка,
// чтобы main не разрастался и чтобы порядок был виден одним куском.

import (
	"context"
	"log"
	"time"

	"github.com/PavluninVladimir/sema-agent/internal/gate"
	semalink "github.com/PavluninVladimir/sema-agent/internal/link"
)

// startGateway — край в интернете. Возвращается сразу: принимать
// звонки Сёмы и запросы браузеров он будет в своих горутинах, как и провод
// у жителя машины.
func startGateway(ctx context.Context, f *agentFlags, conn *semalink.Conn, home string) {
	cfg := gate.Defaults()
	cfg.PublicEndpoint = *f.gateSema
	if *f.gateHTTP != "" {
		cfg.BrowserAddr = *f.gateHTTP
	}
	cfg.Domain = *f.gateDomain
	g := gate.New(cfg, conn)

	// Удостоверение одно на весь агент: берём готовую настройку у провода, а
	// не заводим вторую копию ключа и корня рядом.
	conf, err := conn.TLSConfig()
	if err != nil {
		log.Fatalf("шлюз: %v", err)
	}
	g.TLS(conf)

	// Свой сертификат — запасной путь. Там, где край держит nginx, он не
	// нужен и вреден: отнял бы 443 у живых сайтов.
	if *f.gateACME {
		if err := g.OwnCertificate(*f.gateDomain, home); err != nil {
			log.Fatalf("шлюз: свой сертификат не включается: %v", err)
		}
	}

	// Сёма спрашивает шлюза о себе тем же проводом: домен, край, срок
	// сертификата, сколько отсеяно. Отсев считает он — считать за него
	// значило бы выдумывать.
	for name, h := range g.Handlers() {
		conn.Handle(name, semalink.Handler(h))
	}

	// Мёртвый управляющий провод шлюз обязан заметить сам и быстро: пока он
	// считает его живым, каждый браузер ждёт свой поток 15 секунд и получает
	// 502. Пинг с ответом раз в полминуты — и молчание закрывает провод.
	conn.PingEvery, conn.PingWait = 30*time.Second, 20*time.Second

	go func() {
		if err := g.AcceptSema(ctx, conn.Hold); err != nil {
			log.Fatalf("шлюз: звонки Сёмы принимать не вышло: %v", err)
		}
	}()
	go func() {
		if err := g.Listen(ctx); err != nil {
			log.Fatalf("шлюз: браузеры принимать не вышло: %v", err)
		}
	}()
}
