BINARY := url-text-fetcher
PREFIX ?= /usr/local
BINDIR := $(PREFIX)/bin

.PHONY: build install uninstall test fmt clean

build:
	go build -o $(BINARY) ./cmd/url-text-fetcher

install:
	install -d "$(BINDIR)"
	go build -o "$(BINDIR)/$(BINARY)" ./cmd/url-text-fetcher
	chmod 0755 "$(BINDIR)/$(BINARY)"

uninstall:
	rm -f "$(BINDIR)/$(BINARY)"

test:
	go test ./...

fmt:
	go fmt ./...

clean:
	rm -f "$(BINARY)" "url-text-fetcher-go"
