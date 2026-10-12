CONTAINER ?= podman
TEST_PG    := parley-test-pg
TEST_DB    := postgres://postgres:parley@localhost:55432/parley?sslmode=disable

.PHONY: build test test-unit test-db test-db-stop lint image-api run-api

build:
	go build ./...

lint:
	test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...

# Unit tests only; database tests skip without PARLEY_TEST_DATABASE_URL.
test-unit:
	go test -race ./...

# Full suite against a throwaway Postgres container.
test: test-db
	PARLEY_TEST_DATABASE_URL='$(TEST_DB)' go test -race -count=1 ./...

test-db:
	@$(CONTAINER) container exists $(TEST_PG) 2>/dev/null || \
		$(CONTAINER) run -d --rm --name $(TEST_PG) -e POSTGRES_PASSWORD=parley -e POSTGRES_DB=parley \
			-p 55432:5432 docker.io/library/postgres:17-alpine >/dev/null
	@for i in $$(seq 1 30); do pg_isready -h localhost -p 55432 -q && exit 0; sleep 1; done; echo "postgres did not start"; exit 1

test-db-stop:
	-$(CONTAINER) stop $(TEST_PG)

image-api:
	$(CONTAINER) build -f services/api/Dockerfile -t parley/api:dev .

# Runs the api locally against the test database with an ephemeral signing key.
run-api: test-db
	PARLEY_DATABASE_URL='$(TEST_DB)' PARLEY_SIGNING_KEY="$$(openssl rand -base64 32)" go run ./services/api
