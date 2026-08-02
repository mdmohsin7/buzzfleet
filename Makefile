BIN := bin/buzzfleet
PKG := ./cmd/buzzfleet
# Static by default: the point of a single binary is that it runs on a box with
# no toolchain, no runtime, and nothing to install alongside it.
GOFLAGS := CGO_ENABLED=0
LDFLAGS := -ldflags=-s -ldflags=-w

.PHONY: all build test vet fmt clean cross install

all: build

build:
	cd go && $(GOFLAGS) go build -ldflags="-s -w" -o ../$(BIN) $(PKG)
	@ls -lh $(BIN) | awk '{print "built $(BIN) ("$$5")"}'

test:
	cd go && go test ./...

vet:
	cd go && go vet ./...

fmt:
	cd go && gofmt -l -w .

# The machines people run agents on are mostly linux/amd64 and linux/arm64
# (Oracle, Hetzner, Pi). macOS is here for a laptop acting as a host.
cross:
	@mkdir -p dist
	@for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "  $$os/$$arch"; \
		(cd go && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags="-s -w" \
			-o ../dist/buzzfleet-$$os-$$arch ./cmd/buzzfleet) || exit 1; \
	done
	@ls -lh dist/ | tail -n +2 | awk '{print "  "$$9" "$$5}'

install: build
	./install.sh

clean:
	rm -rf $(BIN) dist
