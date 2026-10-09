BINARY := goroot
PREFIX ?= /usr/local
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build install clean fmt vet test dist

build:
	go build -o $(BINARY) .

install: build
	install -Dm755 $(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)

fmt:
	gofmt -w *.go sandbox/*.go assets/*.go examples/sdk/*.go

vet:
	go vet ./...

test: build
	./scripts/smoke.sh

# Cross-compiled release artifacts in dist/ (see .github/workflows/release.yml).
dist:
	VERSION=$(VERSION) ./scripts/build-release.sh

clean:
	rm -f $(BINARY)
	rm -rf dist
