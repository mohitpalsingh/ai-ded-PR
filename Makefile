BINARY=ai-ded-pr
VERSION=$(shell cat VERSION)

.PHONY: build test clean install regression

build:
	go build -ldflags "-X main.version=$(VERSION)" -o build/$(BINARY) ./cmd/review

test:
	go test ./...
	go vet ./...

regression:
	./scripts/regression.sh

clean:
	rm -rf build

install: build
	./scripts/install.sh
