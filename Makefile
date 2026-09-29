VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test race lint cover demo site clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o pharos ./cmd/pharos

test:
	go test ./...

race:
	go test -race -count=1 ./...

lint:
	gofmt -l . | (! grep .) || (echo "gofmt needed on the files above" && exit 1)
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

demo: build
	./pharos demo

site: build
	./pharos export --demo -o site

clean:
	rm -rf pharos dist site coverage.out
