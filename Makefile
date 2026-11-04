.PHONY: build web test test-integration docker run stop clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/obleey/omegacd/internal/version.Version=$(VERSION)

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/omegacd ./cmd/omegacd

web:
	cd web && npm ci && npm run build

test:
	go test ./...

test-integration:
	go test -tags integration ./...

docker:
	docker build --build-arg VERSION=$(VERSION) -t omegacd:$(VERSION) .

run: build
	OMEGACD_DATA=./data OMEGACD_WORKDIR=$(CURDIR)/data/work OMEGACD_AGE_KEY_FILE=./data/age.key ./bin/omegacd

# Stops only the process LISTENING on the port. Never `kill $$(lsof -ti tcp:8080)`: that also hits
# everything merely connected to it, e.g. OrbStack/Docker Desktop relaying a scrape, or your browser.
stop:
	@pid=$$(lsof -nP -t -iTCP:$${OMEGACD_PORT:-8080} -sTCP:LISTEN); \
	if [ -z "$$pid" ]; then echo "nothing listening on $${OMEGACD_PORT:-8080}"; exit 0; fi; \
	echo "stopping $$(ps -o comm= -p $$pid) (pid $$pid)"; kill $$pid

clean:
	rm -rf bin web/build
