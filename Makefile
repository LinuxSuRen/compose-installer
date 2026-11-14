# compose-installer — pack & install docker compose products
# Build: prefer the unified golang build image (golang:1.25-bookworm) so builds
# are reproducible regardless of the host toolchain; a local go>=1.25 works
# too (build-local). The web UI must be built before the Go binary embeds it.

GO_IMAGE  ?= golang:1.25-bookworm
GOPROXY   ?= direct
BIN_DIR   := bin
DIST_DIR  := dist

.PHONY: all web build build-linux build-local build-all release run clean test lint

all: build

# Web UI: build to web/dist (embedded by the Go binary via //go:embed).
# docs/logo.svg is the single logo source; the copy under web/public is a
# build artifact, not a second source.
web:
	@mkdir -p web/public
	 cp docs/logo.svg web/public/logo.svg 2>/dev/null || true
	pnpm --dir web install
	pnpm --dir web build

# Local build (current platform, bin/compose-installer[.exe])
build-local: web
	go build -trimpath -ldflags "-s -w" -o $(BIN_DIR)/compose-installer .

# Build linux/amd64 inside the golang image (the common delivery form)
build-linux: web
	docker run --rm \
		-v $(CURDIR):/src -w /src \
		-e GOFLAGS=-buildvcs=false -e GOPROXY=$(GOPROXY) \
		-e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=amd64 \
		$(GO_IMAGE) \
		go build -trimpath -ldflags "-s -w" -o $(BIN_DIR)/compose-installer-linux-amd64 .

build: build-linux

# Build every platform: bin/compose-installer-<os>-<arch>[.exe]
build-all: web
	docker run --rm \
		-v $(CURDIR):/src -w /src \
		-e GOFLAGS=-buildvcs=false -e GOPROXY=$(GOPROXY) \
		$(GO_IMAGE) sh scripts/build-all.sh

# Release archives: every platform binary + README packed into tar.gz (dist/)
release: build-all
	@mkdir -p $(DIST_DIR)
	@for f in $(BIN_DIR)/compose-installer-*; do \
		n=$$(basename "$$f"); \
		echo "===> $(DIST_DIR)/$$n.tar.gz"; \
		tar -czf $(DIST_DIR)/$$n.tar.gz -C $(BIN_DIR) "$$n" -C .. README.md; \
	done
	@echo "Release archives:" && ls -lh $(DIST_DIR)

test:
	go test ./...

lint:
	go vet .

run:
	go run . -addr 127.0.0.1:8765

clean:
	rm -rf $(BIN_DIR) $(DIST_DIR) web/dist
	mkdir -p web/dist
	printf '<!doctype html><html lang="en"><p>web UI not built yet: run pnpm install and pnpm build in web/</p></html>' > web/dist/index.html
