default: fmt lint build test

build:
	go build -v ./...

install: build
	go install -v .

lint:
	golangci-lint run

fmt:
	gofmt -s -w -e .

# Regenerates docs/ from the schema, examples/ and templates/.
generate:
	cd tools && go generate ./...

# Unit tests.
test:
	go test -v -cover -timeout=120s ./...

# Acceptance tests against the in-memory WEDOS API fake (no account needed).
testacc:
	TF_ACC=1 go test -v -cover -timeout=20m ./...

# Runs the WEDOS API fake on a local port for manual terraform runs.
mock:
	go run ./cmd/wedos-mock

.PHONY: default build install lint fmt generate test testacc mock
