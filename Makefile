.PHONY: all fmt vet test lint cover clean

PKG          := ./...
COVERPROFILE := coverage.out

all: fmt vet test

fmt:
	go fmt $(PKG)

vet:
	go vet $(PKG)

test:
	go test -race -cover -coverprofile=$(COVERPROFILE) $(PKG)

cover: test
	go tool cover -html=$(COVERPROFILE) -o coverage.html

lint:
	golangci-lint run $(PKG)

clean:
	rm -f $(COVERPROFILE) coverage.html
