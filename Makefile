BINARY := plax
PKG    := ./cmd/plax

.PHONY: build install test vet fmt lint check clean

build:
      go build -o $(BINARY) $(PKG)

install:
      go install $(PKG)

test:
      go test -race ./...

vet:
      go vet ./...

fmt:
      gofmt -l -w .

lint:
      golangci-lint run

check: fmt vet lint test

clean:
      rm -f $(BINARY
