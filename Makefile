.PHONY: all fmt vet test lint cover build build-aarch64 clean

PKG          := ./...
COVERPROFILE := coverage.out
RTL_PROBE    := rtl-probe

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

build:
	go build -o $(RTL_PROBE) ./cmd/rtl-probe

build-aarch64:
	env GOOS=linux GOARCH=arm64 go build -o $(RTL_PROBE)-aarch64 ./cmd/rtl-probe

clean:
	rm -f $(COVERPROFILE) coverage.html $(RTL_PROBE) $(RTL_PROBE)-aarch64
