# uptime-monitor build helpers.
# Pure-Go SQLite driver => CGO can stay off, so binaries are static and
# cross-compile cleanly with no C toolchain.

BINARY := monitor
PKG    := ./cmd/monitor
LDFLAGS := -s -w

.PHONY: build linux linux-arm64 test vet run clean web web-install web-dev

# Build for the current host OS/arch.
build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) $(PKG)

# Static linux/amd64 binary (typical x86-64 server).
linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(BINARY) $(PKG)

# Static linux/arm64 binary (Graviton, Ampere, Raspberry Pi, etc.).
linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o $(BINARY) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

# Run locally with the example seed.
run: build
	./$(BINARY) -seed seed.example.json

# --- admin UI ---------------------------------------------------------------
# The SPA is a separate artefact with its own toolchain: built here, deployed to
# the web server's docroot, and served at the same origin as the API (which sits
# behind /api/). It is not embedded in the binary.

web-install:
	cd web && npm ci

# Production build -> web/dist
web:
	cd web && npm run build

# Vite dev server on :5173, proxying /api to a local monitor on :8080.
web-dev:
	cd web && npm run dev

clean:
	rm -f $(BINARY) $(BINARY).exe
	rm -rf web/dist
