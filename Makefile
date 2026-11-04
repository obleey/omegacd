.PHONY: build web test test-integration docker run clean

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

clean:
	rm -rf bin web/build
