# Go runs through mise so the version pinned in mise.toml is used.
GO ?= mise exec -- go
# Folder that `make install` copies the binary into.
BINDIR ?= $(HOME)/.local/bin

BIN := bin/session-search

.PHONY: build install test

build:
	$(GO) build -o $(BIN) ./cmd/session-search

# Copy the binary, replacing any file or symlink of the same name.
install: build
	mkdir -p "$(BINDIR)"
	rm -f "$(BINDIR)/session-search"
	install -m 0755 $(BIN) "$(BINDIR)/session-search"

test:
	$(GO) test ./...
