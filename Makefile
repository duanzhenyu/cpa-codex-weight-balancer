PLUGIN  := cpa-codex-weight-balancer
VERSION := 0.1.0
GOOS    ?= $(shell go env GOOS)
GOARCH  ?= $(shell go env GOARCH)
EXT     := $(if $(filter windows,$(GOOS)),.dll,$(if $(filter darwin,$(GOOS)),.dylib,.so))
DIST    := dist

.PHONY: all build test vet clean release checksums

all: vet test build

build:
	@mkdir -p $(DIST)
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build -trimpath -ldflags="-s -w" -buildmode=c-shared \
		-o $(DIST)/$(PLUGIN)$(EXT) ./plugin

test:
	go test ./...

vet:
	go vet ./...

# The dynamic library must sit at the zip root and be named after the plugin ID;
# nested paths or a mismatched name are rejected by the installer.
release: build
	@cd $(DIST) && zip -q -j $(PLUGIN)_$(VERSION)_$(GOOS)_$(GOARCH).zip $(PLUGIN)$(EXT)
	@echo "$(DIST)/$(PLUGIN)_$(VERSION)_$(GOOS)_$(GOARCH).zip"

checksums:
	@cd $(DIST) && sha256sum *.zip > checksums.txt && cat checksums.txt

clean:
	rm -rf $(DIST) build
