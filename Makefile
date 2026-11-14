BINARY := plax
PKG    := ./cmd/plax

.PHONY: build install test vet fmt lint check clean

build:
	go build -o $(BINARY) $(PKG)

install:
	go install $(PKG)

test:
	@if [ -n "$$PLAX_TEST_POSTGRES_URL" ]; then \
		printf '%s\n' 'tests: Postgres + CLI E2E enabled; serial package execution (-p=1).' >&2; \
		go test -race -count=1 -timeout=10m -p=1 ./...; \
	else \
		printf '%s\n' 'tests: Postgres disabled (PLAX_TEST_POSTGRES_URL unset/empty); skipping Postgres integration and CLI E2E.' >&2; \
		go test -race -count=1 -timeout=10m ./...; \
	fi

vet:
	go vet ./...

fmt:
	gofmt -l -w .

lint:
	golangci-lint run

check: fmt vet lint test

clean:
	rm -f $(BINARY)
