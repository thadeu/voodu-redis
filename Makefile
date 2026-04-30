# voodu-redis — Makefile
#
# Build targets produce the `voodu-redis` binary under bin/, which
# is the real plugin command. The shell wrappers (bin/expand)
# exec it with the right subcommand — that's how the Voodu plugin
# loader discovers each command by name.

BIN      := bin/voodu-redis
PKG      := ./cmd/voodu-redis
DIST     := dist
VERSION  := $(shell grep '^version:' plugin.yml | awk '{print $$2}')

GO       := go
LDFLAGS  := -s -w -X main.version=$(VERSION)

.PHONY: build test lint cross clean install-local

build:
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN) $(PKG)

test:
	$(GO) test ./...

lint:
	$(GO) vet ./...

cross: $(DIST)/voodu-redis_linux_amd64 $(DIST)/voodu-redis_linux_arm64

$(DIST)/voodu-redis_linux_amd64:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -ldflags '$(LDFLAGS)' -o $@ $(PKG)

$(DIST)/voodu-redis_linux_arm64:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -ldflags '$(LDFLAGS)' -o $@ $(PKG)

install-local: build
	@if [ -z "$(PLUGINS_ROOT)" ]; then \
		echo "PLUGINS_ROOT is required (e.g. /opt/voodu/plugins)"; exit 1; \
	fi
	@mkdir -p $(PLUGINS_ROOT)/redis/bin
	cp $(BIN) $(PLUGINS_ROOT)/redis/bin/voodu-redis
	cp bin/expand $(PLUGINS_ROOT)/redis/bin/
	chmod +x $(PLUGINS_ROOT)/redis/bin/*
	cp plugin.yml $(PLUGINS_ROOT)/redis/
	cp install uninstall $(PLUGINS_ROOT)/redis/ 2>/dev/null || true
	@echo "installed into $(PLUGINS_ROOT)/redis"

clean:
	rm -rf $(BIN) $(DIST)
