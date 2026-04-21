BINARY      := clcli
BUILD_DIR   := build
VERSION     := $(shell git describe --tags --always 2>/dev/null || echo "v0.4.0-dev")
# make
# -s  strip the symbol table
# -w  strip DWARF debug info
# These two alone typically shave 25-30% off the binary.
LDFLAGS     := -ldflags "-s -w -X main.Version=$(VERSION)"

# -trimpath removes absolute filesystem paths from the binary (privacy + a few KB).
BUILDFLAGS  := -trimpath $(LDFLAGS)

.PHONY: build build-windows build-linux build-darwin build-all release compress install test tidy clean size

build:
	CGO_ENABLED=0 go build $(BUILDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/clcli/

build-windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(BUILDFLAGS) -o $(BUILD_DIR)/$(BINARY).exe ./cmd/clcli/

build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(BUILDFLAGS) -o $(BUILD_DIR)/$(BINARY)-linux ./cmd/clcli/

build-darwin:
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build $(BUILDFLAGS) -o $(BUILD_DIR)/$(BINARY)-darwin ./cmd/clcli/
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build $(BUILDFLAGS) -o $(BUILD_DIR)/$(BINARY)-darwin-arm64 ./cmd/clcli/

build-all: build-windows build-linux build-darwin

# release: build everything + compress with UPX if available.
# UPX typically cuts the final size by another 50-65%.
# Install UPX: https://upx.github.io/  (choco install upx / brew install upx / apt install upx-ucl)
release: build-all compress size

compress:
	@command -v upx >/dev/null 2>&1 || { echo "!! upx not installed — skipping compression."; exit 0; }
	@for f in $(BUILD_DIR)/$(BINARY)*; do \
		echo "compressing $$f..."; \
		upx --best --lzma "$$f" 2>/dev/null || echo "  skipped $$f"; \
	done

install:
	CGO_ENABLED=0 go install $(BUILDFLAGS) ./cmd/clcli/

size:
	@echo ""
	@echo "Binary sizes:"
	@ls -lh $(BUILD_DIR)/ 2>/dev/null | awk 'NR>1 {printf "  %-30s %s\n", $$NF, $$5}' || \
		dir $(BUILD_DIR) 2>nul

test:
	go test -v ./...

tidy:
	go mod tidy

clean:
	rm -rf $(BUILD_DIR) 2>/dev/null || rmdir /s /q $(BUILD_DIR)
