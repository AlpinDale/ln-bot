BINARY  := lnbot
MODULE  := github.com/alpindale/ln-bot

.PHONY: build run test vet lint tidy docker docker-run clean

build:
	CGO_ENABLED=0 go build -o bin/$(BINARY) ./cmd/lnbot

run:
	go run ./cmd/lnbot

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy

docker:
	docker build -t ln-bot:latest .

docker-run:
	docker compose up -d

clean:
	rm -rf bin
