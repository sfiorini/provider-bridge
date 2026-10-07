.PHONY: test cover cover-html cover-check build webui-install webui-test webui-build build-with-webui release

COVERAGE_THRESHOLD := 95
COVER_PROFILE := /tmp/providerbridge-coverage.out

VERSION     ?= $(shell cat VERSION 2>/dev/null || echo dev)
BUILD_TIME  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
GO_VERSION  := $(shell go env GOVERSION 2>/dev/null || echo unknown)
LDFLAGS     := -X providerbridge/internal/service/api.version=$(VERSION) \
               -X providerbridge/internal/service/api.buildTime=$(BUILD_TIME) \
               -X providerbridge/internal/service/api.goVersion=$(GO_VERSION)

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" ./...

webui-install:
	npm --prefix webui install

webui-test:
	npm --prefix webui test

webui-build:
	npm --prefix webui run build
	rm -rf internal/service/webui/dist
	mkdir -p internal/service/webui/dist
	cp -R webui/dist/. internal/service/webui/dist/

build-with-webui: webui-build
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" ./...

test:
	CGO_ENABLED=0 go test ./...

cover:
	CGO_ENABLED=0 go test -cover ./...

cover-check:
	@echo "Checking per-package coverage (threshold: $(COVERAGE_THRESHOLD)%)..."
	@fail=0; \
	for pkg in $$(CGO_ENABLED=0 go test -cover ./... 2>&1 | grep 'coverage:' | grep -v '0.0%' | grep -v 'no statements'); do \
		echo "$$pkg"; \
	done; \
	echo ""; \
	echo "--- Enforced packages ---"; \
	for pkg in internal/extension/plugin; do \
		pct=$$(CGO_ENABLED=0 go test -cover ./$$pkg/ 2>&1 | grep -oP '[0-9]+\.[0-9]+(?=%)'); \
		echo "$$pkg: $${pct}%"; \
		if [ $$(echo "$${pct} < $(COVERAGE_THRESHOLD)" | bc -l) -eq 1 ]; then \
			echo "  FAIL: $${pct}% < $(COVERAGE_THRESHOLD)%"; \
			fail=1; \
		fi; \
	done; \
	if [ $$fail -eq 1 ]; then echo "Coverage check FAILED"; exit 1; fi; \
	echo "Coverage check PASSED"

release:
	./scripts/release.sh
