PLUGIN_NAME ?= mihomo-egress
VERSION ?= 0.1.1
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
BUILD_DIR ?= dist/$(GOOS)/$(GOARCH)
PLUGIN_EXT_linux = so
PLUGIN_EXT_freebsd = so
PLUGIN_EXT_darwin = dylib
PLUGIN_EXT_windows = dll
PLUGIN_EXT = $(or $(PLUGIN_EXT_$(GOOS)),so)
PLUGIN_OUTPUT ?= $(BUILD_DIR)/$(PLUGIN_NAME).$(PLUGIN_EXT)
ARCHIVE ?= dist/$(PLUGIN_NAME)_$(VERSION)_$(GOOS)_$(GOARCH).zip
GO_LDFLAGS ?= -s -w -X main.pluginVersion=$(VERSION)
SHA256SUM ?= $(shell command -v sha256sum 2>/dev/null || command -v shasum)
SHA256FLAGS ?= $(if $(findstring shasum,$(SHA256SUM)),-a 256,)

.PHONY: test vet build build-linux-amd64 package clean

test:
	go test ./...

vet:
	go vet ./...

build:
	mkdir -p $(dir $(PLUGIN_OUTPUT))
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -buildmode=c-shared -ldflags "$(GO_LDFLAGS)" -o $(PLUGIN_OUTPUT) .
	rm -f $(PLUGIN_OUTPUT).h

build-linux-amd64:
	$(MAKE) build GOOS=linux GOARCH=amd64 PLUGIN_OUTPUT=dist/linux/amd64/$(PLUGIN_NAME).so

package: build
	mkdir -p dist
	cd $(dir $(PLUGIN_OUTPUT)) && zip -q -j "$(abspath $(ARCHIVE))" "$(notdir $(PLUGIN_OUTPUT))"
	cd $(dir $(ARCHIVE)) && $(SHA256SUM) $(SHA256FLAGS) "$(notdir $(ARCHIVE))" > "$(notdir $(ARCHIVE)).sha256"

clean:
	rm -rf dist
