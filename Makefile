SHELL := /bin/sh

GO ?= go
HELM ?= helm
GOVULNCHECK ?= govulncheck
VERSION ?= 0.1.0-alpha.1
LDFLAGS := -s -w -X main.version=$(VERSION)
CHART := deploy/helm/skopos

.PHONY: help build test test-race test-integration fuzz vet vuln helm-lint helm-template check clean e2e-obi compat-cerberus bench release release-dry-run release-check

help:
	@printf '%s\n' \
		'build             build the gateway binary' \
		'test              run unit tests' \
		'test-race         run unit tests with the race detector' \
		'test-integration  run integration tests (requires CLICKHOUSE_URL)' \
		'fuzz              run the SQL emitter fuzz target for 30s' \
		'vet               run go vet' \
		'vuln              run govulncheck' \
		'helm-lint         lint the Helm chart' \
		'helm-template     render the Helm chart' \
		'e2e-obi           run the real OBI end-to-end harness (Docker, Linux)' \
		'compat-cerberus   run the cerberus three-way differential' \
		'bench             run the ClickHouse compression and gateway latency benchmark' \
		'release           build and package release artifacts (VERSION=...)' \
		'release-dry-run   release build without pushing or signing' \
		'release-check     release preflight: vet, tests, Helm, script syntax' \
		'check             vet + test + fuzz + helm lint'

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

vuln:
	$(GOVULNCHECK) ./...

helm-lint:
	$(HELM) lint $(CHART)

helm-template:
	$(HELM) template skopos $(CHART) --namespace observability

check: vet test helm-lint fuzz

e2e-obi:
	./test/e2e/obi/run.sh

compat-cerberus:
	./test/compat/cerberus/run.sh

bench:
	./test/bench/run.sh

release:
	VERSION=$(VERSION) ./scripts/release.sh

release-dry-run:
	VERSION=$(VERSION) PUSH=0 SIGN=0 ./scripts/release.sh

release-check: vet test helm-lint
	$(HELM) template skopos $(CHART) --namespace observability >/dev/null
	cd test/compat/prometheus && $(GO) mod verify && $(GO) vet ./... && $(GO) test -count=1 ./...
	sh -n scripts/release.sh
	sh -n scripts/sbom-fallback.sh
	sh -n test/e2e/obi/run.sh
	sh -n test/e2e/obi/verify.sh
	sh -n test/e2e/obi/load.sh
	sh -n test/e2e/obi/render-schema.sh
	sh -n test/e2e/obi/init-clickhouse.sh
	sh -n test/bench/run.sh
	sh -n test/bench/measure-compression.sh
	@printf '%s\n' 'release-check: ok'

clean:
	rm -rf bin
