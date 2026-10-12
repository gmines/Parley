CONTAINER ?= podman
TEST_PG    := parley-test-pg
TEST_DB    := postgres://postgres:parley@localhost:55432/parley?sslmode=disable

.PHONY: build test test-unit test-db test-db-stop dev-nats lint image-api image-realtime image-voice-control image-asterisk image-kamailio image-pjsua run-api

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
			-p 127.0.0.1:55432:5432 docker.io/library/postgres:17-alpine >/dev/null
	@for i in $$(seq 1 30); do pg_isready -h localhost -p 55432 -q && exit 0; sleep 1; done; echo "postgres did not start"; exit 1

# NATS for realtime events (localhost only).
dev-nats:
	@$(CONTAINER) container exists parley-nats 2>/dev/null || \
		$(CONTAINER) run -d --rm --name parley-nats -p 127.0.0.1:4222:4222 docker.io/library/nats:2 >/dev/null

test-db-stop:
	-$(CONTAINER) stop $(TEST_PG)

image-api:
	$(CONTAINER) build -f services/api/Dockerfile -t parley/api:dev .

image-realtime:
	$(CONTAINER) build -f services/realtime/Dockerfile -t parley/realtime:dev .

image-voice-control:
	$(CONTAINER) build -f services/voice-control/Dockerfile -t parley/voice-control:dev .

# Voice media stack (see voice/README.md; WITH_OPUS=0 for an all-open-source build).
WITH_OPUS ?= 1
image-asterisk:
	$(CONTAINER) build --build-arg WITH_OPUS=$(WITH_OPUS) -f voice/asterisk/Containerfile -t parley/asterisk:dev voice/asterisk

image-kamailio:
	$(CONTAINER) build -f voice/kamailio/Containerfile -t parley/kamailio:dev voice/kamailio

# Test-only SIP client for voice/test/call_test.py.
image-pjsua:
	$(CONTAINER) build -f voice/test/pjsua.Containerfile -t parley/pjsua:dev voice/test

# Runs the api locally against the test database with an ephemeral signing key.
run-api: test-db
	PARLEY_DATABASE_URL='$(TEST_DB)' PARLEY_SIGNING_KEY="$$(openssl rand -base64 32)" go run ./services/api
