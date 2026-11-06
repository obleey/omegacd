.PHONY: build web test test-integration e2e docker run stop up down clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/obleey/omegacd/internal/version.Version=$(VERSION)
# where `make run` writes Application files; override with make run OMEGACD_WORKDIR=/some/dir
OMEGACD_WORKDIR ?= $(CURDIR)/data/work

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/omegacd ./cmd/omegacd

web:
	cd web && npm ci && npm run build

test:
	go test ./...

test-integration:
	go test -tags integration ./...

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
