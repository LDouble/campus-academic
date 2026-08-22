.PHONY: build test test-race vet fmt generate generate-check migration-up

build:
	go build ./cmd/academic-provider ./cmd/academic-analytics ./cmd/academicctl

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal

generate:
	buf lint
	buf generate
	go generate ./internal/infrastructure/mysql

generate-check:
	buf lint
	buf breaking --against '.git#branch=master'
	go generate ./internal/infrastructure/mysql
	git diff --exit-code -- pkg internal/infrastructure/mysql/query

migration-up:
	go run ./cmd/academicctl migrate up
