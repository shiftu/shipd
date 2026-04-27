VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build run tidy test clean docker

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o shipd ./cmd/shipd

run: build
	./shipd serve --data-dir ./data

tidy:
	go mod tidy

test:
	go test ./...

clean:
	rm -f shipd
	rm -rf data dist

docker:
	docker build -t shipd:$(VERSION) .
