BINARY := goroot
PREFIX ?= /usr/local

.PHONY: build install clean fmt vet test

build:
	go build -o $(BINARY) .

install: build
	install -Dm755 $(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)

fmt:
	gofmt -w *.go

vet:
	go vet ./...

test: build
	./scripts/smoke.sh

clean:
	rm -f $(BINARY)
