.PHONY: build web test test-integration run clean

build: web
	CGO_ENABLED=0 go build -o bin/dockercd ./cmd/dockercd

web:
	cd web && npm ci && npm run build

test:
	go test ./...

test-integration:
	go test -tags integration ./...

run: build
	DOCKERCD_DATA=./data DOCKERCD_WORKDIR=$(CURDIR)/data/work DOCKERCD_AGE_KEY_FILE=./data/age.key ./bin/dockercd

clean:
	rm -rf bin web/build
