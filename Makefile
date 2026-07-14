# uptime-monitor build helpers.
# Pure-Go SQLite driver => CGO can stay off, so binaries are static and
# cross-compile cleanly with no C toolchain.

BINARY := monitor
PKG    := ./cmd/monitor
LDFLAGS := -s -w

.PHONY: build linux linux-arm64 test vet run clean

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

clean:
	rm -f $(BINARY) $(BINARY).exe
