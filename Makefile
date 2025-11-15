.PHONY: all clean client server

VERSION ?= $(shell git rev-parse --short=6 HEAD)$(if $(shell git status --porcelain),-dev)

all: client server

client:
	CGO_ENABLED=0 go build -ldflags='-s -w -X main.version=$(VERSION)' -trimpath -o ./bin/client ./cmd/client/main.go

server:
	CGO_ENABLED=0 go build -ldflags='-s -w -X main.version=$(VERSION)' -trimpath -o ./bin/server ./cmd/server/main.go

clean:
	rm -rf ./bin

docker:
	docker build -t ghcr.io/zeyugao/synapse:latest . --build-arg VERSION=$(VERSION)
