SHELL := /bin/sh

GO ?= go
HELM ?= helm
VERSION ?= 0.1.0-alpha.1
LDFLAGS := -s -w -X main.version=$(VERSION)
CHART := deploy/helm/skopos

.PHONY: help build test test-race test-integration fuzz vet helm-lint helm-template check clean

help:
	@printf '%s\n' \
		'build             build the gateway binary' \
		'test              run unit tests' \
		'test-race         run unit tests with the race detector' \
		'test-integration  run integration tests (requires CLICKHOUSE_URL)' \
		'fuzz              run the SQL emitter fuzz target for 30s' \
		'vet               run go vet' \
		'helm-lint         lint the Helm chart' \
		'helm-template     render the Helm chart' \
		'check             vet + test + helm lint'

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/gateway ./cmd/gateway

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

test-integration:
	@test -n "$$CLICKHOUSE_URL" || (echo "CLICKHOUSE_URL is required" >&2; exit 1)
	$(GO) test -tags=integration ./...

fuzz:
	$(GO) test -run='^$$' -fuzz=FuzzEmitNoUserValueLeak -fuzztime=30s ./internal/sqlgen

vet:
	$(GO) vet ./...

helm-lint:
	$(HELM) lint $(CHART)

helm-template:
	$(HELM) template skopos $(CHART) --namespace observability

check: vet test helm-lint

clean:
	rm -rf bin
