# Сборка жителя машины (semaagent). Бинарник кросс-компилируется, а не
# собирается на месте: обновление на Linux — замена файла и перезапуск
# службы, ни Go, ни исходники там не нужны (design.md, решение 4).

BIN := semaagent
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.Version=$(VERSION)

.PHONY: build linux linux-amd64 linux-arm64 mac fmt vet test clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/semaagent

# обе архитектуры разом — Pi под ARM, VPS обычно amd64
linux: linux-amd64 linux-arm64

linux-amd64:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-linux-amd64 ./cmd/semaagent

linux-arm64:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-linux-arm64 ./cmd/semaagent

mac:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-darwin-$(shell go env GOARCH) ./cmd/semaagent

fmt:
	gofmt -l .

vet:
	go vet ./...

test:
	go test ./...

clean:
	rm -rf dist $(BIN)
