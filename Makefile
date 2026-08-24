.PHONY: build test test-race vet fmt generate generate-check migration-up production review analytics-production analytics-review test-deploy test-deploy-provider test-deploy-analytics test-composite-image

BUF ?= go run github.com/bufbuild/buf/cmd/buf@v1.61.0

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
	$(BUF) lint proto
	$(BUF) generate proto
	go generate ./internal/infrastructure/mysql

generate-check:
	$(BUF) lint proto
	$(BUF) breaking --against '.git#branch=master' proto
	go generate ./internal/infrastructure/mysql
	git diff --exit-code -- pkg internal/infrastructure/mysql/query

migration-up:
	go run ./cmd/academicctl migrate up

production:
	./scripts/deploy-provider.sh production

review:
	./scripts/deploy-provider.sh review

analytics-production:
	./scripts/deploy-analytics.sh production

analytics-review:
	./scripts/deploy-analytics.sh review

test-deploy: test-deploy-provider test-deploy-analytics test-composite-image

test-deploy-provider:
	./scripts/tests/deploy-provider_test.sh

test-deploy-analytics:
	./scripts/tests/deploy-analytics_test.sh

test-composite-image:
	./scripts/tests/composite-image_test.sh
