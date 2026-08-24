.PHONY: build test test-race vet fmt generate generate-check migration-up production review analytics-production analytics-review test-deploy test-deploy-provider test-deploy-analytics test-composite-image test-provider-config test-render-provider-config bootstrap-render-provider-config bootstrap-prepare deploy-bootstrap deploy-validate

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

test-deploy: test-deploy-provider test-deploy-analytics test-composite-image test-provider-config test-render-provider-config

test-deploy-provider:
	./scripts/tests/deploy-provider_test.sh

test-deploy-analytics:
	./scripts/tests/deploy-analytics_test.sh

test-composite-image:
	./scripts/tests/composite-image_test.sh

test-provider-config:
	./scripts/tests/provider-config_test.sh

test-render-provider-config:
	./scripts/tests/render-provider-config_test.sh

bootstrap-render-provider-config:
	@test -n "$(ENVIRONMENT)" && test -n "$(OUTPUT)" || (echo 'usage: make bootstrap-render-provider-config ENVIRONMENT=review|production OUTPUT=/absolute/provider-config.yaml' >&2; exit 2)
	./scripts/render-provider-config.sh "$(ENVIRONMENT)" "$(OUTPUT)"

bootstrap-prepare:
	@test -n "$(ENVIRONMENT)" || (echo 'usage: make bootstrap-prepare ENVIRONMENT=review|production' >&2; exit 2)
	CAMPUS_DEPLOY_STATE_DIR="$(DEPLOY_STATE_DIR)" ./scripts/prepare-bootstrap.sh "$(ENVIRONMENT)"

deploy-bootstrap:
	@test -n "$(ENVIRONMENT)" && test -n "$(ROLE)" && test -n "$(TARGET)" || (echo 'usage: make deploy-bootstrap ENVIRONMENT=review|production ROLE=provider|analytics TARGET=user@host' >&2; exit 2)
	./scripts/bootstrap-role.sh "$(ENVIRONMENT)" "$(ROLE)" "$(TARGET)"

deploy-validate:
	@test -n "$(ENVIRONMENT)" || (echo 'usage: make deploy-validate ENVIRONMENT=review|production' >&2; exit 2)
	./scripts/validate-bootstrap.sh "$(ENVIRONMENT)"
