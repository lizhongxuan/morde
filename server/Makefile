.PHONY: build smoke lint test race cover generate it

build:
	go build ./...

# smoke: P0 smoke 10 deploy/merge gate (E10). Runs the ten P0 cases
# (T01/T04/T06/T10/T13/T19/T22/T26/T36/T43) against the current checkout
# and writes deploy/single/smoke-record.json. Exit non-zero on any
# failure. Run this before merging to sit and before any deploy; the
# deploy script runs it automatically.
smoke:
	@bash ../deploy/single/smoke.sh --record ../deploy/single/smoke-record.json

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run ./cmd/... ./internal/... ./migrations

test:
	go test ./...

race:
	go test -race ./...

cover:
	@set -eu; profile=$$(mktemp); trap 'rm -f "$$profile"' EXIT; \
	packages=$$(go list ./internal/... | grep -v '/sqlcgen$$' || true); \
	if [ -z "$$packages" ]; then echo 'no internal packages'; exit 0; fi; \
	go test -p=1 -timeout=30m -coverprofile="$$profile" $$packages; \
	go tool cover -func="$$profile" | tail -n 1

generate:
	@if [ ! -f sqlc.yaml ]; then echo 'sqlc.yaml not found; skipping sqlc generation'; exit 0; fi
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate

it:
	@if [ -z "$${CH_TEST_DATABASE_URL:-}" ]; then echo 'skipping PostgreSQL integration tests: CH_TEST_DATABASE_URL is not set'; fi
	@if [ -z "$${CH_OSB_URL:-}" ] || [ -z "$${CH_OSB_API_KEY:-}" ]; then echo 'skipping OpenSandbox integration tests: CH_OSB_URL and CH_OSB_API_KEY are not both set'; fi
	go test -timeout=45m ./...
