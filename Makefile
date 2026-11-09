.PHONY: build web proto test test-integration test-engines test-all test-linux cover e2e docker run stop up down clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/obleey/omegacd/internal/version.Version=$(VERSION)
# where `make run` writes Application files; override with make run OMEGACD_WORKDIR=/some/dir
OMEGACD_WORKDIR ?= $(CURDIR)/data/work

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/omegacd ./cmd/omegacd

web:
	cd web && npm ci && npm run build

# Generated code is committed; CI checks that this leaves it unchanged.
BUF := go run github.com/bufbuild/buf/cmd/buf@v1.75.0
proto:
	$(BUF) lint
	$(BUF) generate

test:
	go test ./...

test-integration:
	go test -tags integration ./...

# the Agent's integration tests against each engine it supports (test/engines.sh starts the rootless ones)
ENGINES_ROOT ?= $(abspath $(or $(TMPDIR),/tmp))/omegacd-engines
# ENGINES picks some: test/linux.sh adds host-podman (the machine's own rootless Podman, run as the test's user:
# Q58's rootless case) on every machine
ENGINES ?= rootful rootless podman
test-engines:
	test/engines.sh up $(ENGINES_ROOT)
	$(if $(filter rootful,$(ENGINES)),go test -tags integration -count=1 $(call cov,engines-rootful) -run TestAgent ./internal/agent)
	$(if $(filter rootless,$(ENGINES)),DOCKER_HOST=tcp://127.0.0.1:23750 OMEGACD_IT_ROOT=$(ENGINES_ROOT) go test -tags integration -count=1 $(call cov,engines-rootless) -run TestAgent ./internal/agent)
	$(if $(filter podman,$(ENGINES)),DOCKER_HOST=tcp://127.0.0.1:23751 OMEGACD_IT_ROOT=$(ENGINES_ROOT) go test -tags integration -count=1 $(call cov,engines-podman) -run TestAgent ./internal/agent)
	$(if $(filter host-podman,$(ENGINES)),DOCKER_HOST=unix://$$XDG_RUNTIME_DIR/podman/podman.sock go test -tags integration -count=1 $(call cov,engines-host-podman) -run TestAgent ./internal/agent)
	@echo "engines tested: $(ENGINES)"

# the whole suite, as before every PR (web/build must exist: make web): vet, unit with -race, integration, the Agent on
# every engine, end-to-end with Forgejo
test-all: export OMEGACD_TEST_ALL = 1 # a test whose environment is missing fails here instead of skipping
test-all:
	go vet ./... && go vet -tags integration ./...
	go test -race -count=1 $(call cov,unit) ./...
	go test -count=1 -tags integration -timeout 30m $(call cov,integration) ./...
	$(MAKE) test-engines
	docker compose -f test/e2e/forgejo.yaml up -d --wait --force-recreate
	CGO_ENABLED=0 go build $(if $(COVER_DIR),-cover -coverpkg=$(COVERPKG)) -o bin/omegacd-$$(go env GOARCH) ./cmd/omegacd
	$(if $(COVER_DIR),mkdir -p $(COVER_DIR)/e2e && GOCOVERDIR=$(COVER_DIR)/e2e) OMEGACD_BIN=$(CURDIR)/bin/omegacd-$$(go env GOARCH) test/e2e/run.sh
	$(if $(COVER_DIR),go tool covdata textfmt -i=$(COVER_DIR)/e2e -o $(COVER_DIR)/e2e.out)

# Coverage (Q96): COVER_DIR=dir makes test-all write a profile per kind of test there (unit, integration, each engine,
# and the e2e scripts' binary built with -cover); generated protobuf code is left out. make cover runs it and checks:
# blocks no test reaches fail where a line of them changed since COVER_BASE (unless marked "coverage: unreachable,
# <reason>" in the code), and the total may not drop below test/coverage-floor.
COVERPKG = $(shell go list ./cmd/... ./internal/... | grep -v /internal/proto/ | paste -sd, -)
cov = $(if $(COVER_DIR),-coverpkg=$(COVERPKG) -coverprofile=$(COVER_DIR)/$(1).out)
COVER_BASE ?= origin/main
cover:
	rm -rf cover && mkdir -p cover
	$(MAKE) test-all COVER_DIR=$(CURDIR)/cover
	go run ./test/coverage -base $(COVER_BASE) -floor test/coverage-floor cover/*.out

# test-all on the supported Linux Hosts: Ubuntu 24.04 and Debian 13, amd64 and arm64, the same tests on each
# (test/linux.sh: Hetzner servers from ../omegacd-testinfra)
test-linux:
	test/linux.sh

# end-to-end: throwaway OmegaCDs driven through the CLI against the local Docker engine (see test/e2e/run.sh)
e2e: build
	test/e2e/run.sh

docker:
	docker build --build-arg VERSION=$(VERSION) -t omegacd:$(VERSION) .

run: build
	OMEGACD_DATA=./data OMEGACD_WORKDIR=$(OMEGACD_WORKDIR) OMEGACD_AGE_KEY_FILE=./data/age.key ./bin/omegacd

# Stops only the process LISTENING on the port. Never `kill $$(lsof -ti tcp:8080)`: that also hits
# everything merely connected to it, e.g. OrbStack/Docker Desktop relaying a scrape, or your browser.
stop:
	@pid=$$(lsof -nP -t -iTCP:$${OMEGACD_PORT:-8080} -sTCP:LISTEN); \
	if [ -z "$$pid" ]; then echo "nothing listening on $${OMEGACD_PORT:-8080}"; exit 0; fi; \
	echo "stopping $$(ps -o comm= -p $$pid) (pid $$pid)"; kill $$pid

# Runs OmegaCD as a container (deploy/compose.yaml) from this checkout, with the same data and work dir as
# `make run`; stop `make run` first, both use port 8080.
up: docker
	touch data/known_hosts
	OMEGACD_IMAGE=omegacd:$(VERSION) OMEGACD_DATA=$(CURDIR)/data OMEGACD_WORKDIR=$(OMEGACD_WORKDIR) \
	OMEGACD_AGE_KEY=$(CURDIR)/data/age.key OMEGACD_KNOWN_HOSTS=$(CURDIR)/data/known_hosts \
	docker compose -f deploy/compose.yaml up -d

down:
	docker compose -f deploy/compose.yaml down

clean:
	rm -rf bin web/build
