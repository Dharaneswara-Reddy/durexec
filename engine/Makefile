# Operational entry points for the durexec engine: the demonstration
# cluster, the usual Go hygiene commands, and the commands that make the
# project's claims checkable — kill-owner (force a failover), verify (prove
# no event was ever double-appended while it happened), check-replication
# (prove the standby is a real, working replica), promote-drill (prove the
# standby can actually take over) and experiment (measure
# semantic dedup against a real embedding model instead of the built-in
# hash-based stand-in).
#
# Every target below is safe to run more than once in a row.

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

COMPOSE_FILE := deploy/docker-compose.yml
COMPOSE      := docker compose -f $(COMPOSE_FILE)

# The port of a node to talk to for one-shot requests (kill-owner, demo).
# Any node answers /v1/shards for the whole cluster and routes workflow
# requests to the correct owner itself, so which one we pick does not matter.
NODE_PORT   ?= 8081
BASE_URL    ?= http://localhost:$(NODE_PORT)

# The leader's port is published on the host as 5433 (see
# deploy/docker-compose.yml) so migrate/verify can run without a shell inside
# a container.
DATABASE_URL ?= postgres://durexec:durexec@localhost:5433/durexec?sslmode=disable

# The standby's port is published on the host as 5434 (see
# deploy/docker-compose.yml), for the same reason: check-replication reads
# it directly with psql, no shell inside the container needed.
STANDBY_DATABASE_URL ?= postgres://durexec:durexec@localhost:5434/durexec?sslmode=disable

# The embedder's port is published on the host as 8000 (see
# deploy/docker-compose.yml), so `make experiment` can point a plain
# `go run` at the real model server without a shell inside its container.
EXPERIMENT_ENDPOINT ?= http://localhost:8000/embed

# promote-drill puts the cluster back the way `make up` leaves it unless this
# is 0.
RESTORE ?= 1

.PHONY: up down logs migrate test vet fmt lint build kill-owner demo verify \
	check-replication promote-drill embedder-logs experiment

## up: build and start the demonstration cluster (3 engine nodes, leader +
## standby Postgres, Prometheus, Grafana), waiting for every healthcheck.
up-semantic: ## base stack plus the real all-MiniLM-L6-v2 embedder (3200 MB)
	$(COMPOSE) -f deploy/docker-compose.embedder.yml up -d --build --wait

up:
	$(COMPOSE) up -d --build --wait

## down: stop the cluster. Data volumes are kept, so a following `make up`
## resumes rather than re-clones the standby from scratch.
down:
	$(COMPOSE) down

## logs: follow every service's logs.
logs:
	$(COMPOSE) logs -f --tail=200

## migrate: apply migrations/0001_init.up.sql to DATABASE_URL. Every
## statement in that file is CREATE ... IF NOT EXISTS, so re-running this
## against an already-migrated database is a no-op, not an error.
migrate:
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -f migrations/0001_init.up.sql

## test: run every package's own unit tests. Tests that need a real
## PostgreSQL skip themselves unless DUREXEC_TEST_DSN is set (see
## internal/*/integration_test.go); export it to include them.
test:
	go test ./...

## vet: go vet across the whole module.
vet:
	go vet ./...

## fmt: gofmt every source file in place.
fmt:
	gofmt -l -w .

## lint: golangci-lint if it is installed, otherwise the gofmt+vet check
## that's the minimum bar CI enforces.
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "lint: golangci-lint not found, falling back to gofmt -l + go vet"; \
		unformatted="$$(gofmt -l .)"; \
		if [ -n "$$unformatted" ]; then \
			echo "lint: the following files are not gofmt-clean:"; \
			echo "$$unformatted"; \
			exit 1; \
		fi; \
		go vet ./...; \
	fi

## build: compile every package and command.
build:
	go build ./...

## kill-owner: find whoever currently owns shard 2 (via GET /v1/shards, which
## any node answers for the whole cluster) and `docker kill` that container,
## to force the failover the safety argument depends on. Container names
## match NODE_ID exactly (node-a/node-b/node-c), so the owner name returned
## by the API is also the container to kill — no translation table needed.
kill-owner:
	@owner="$$(curl -sf $(BASE_URL)/v1/shards | jq -r '.shards[] | select(.shard_id == 2) | .owner')"; \
	if [ -z "$$owner" ] || [ "$$owner" = "null" ]; then \
		echo "kill-owner: shard 2 has no current owner (lease unowned or expired) — nothing to kill"; \
		exit 1; \
	fi; \
	echo "kill-owner: shard 2 is owned by $$owner — killing it"; \
	docker kill "$$owner"; \
	echo "kill-owner: killed. LEASE_TTL is 10s; watch \`make logs\` or Grafana's shard_recovery_seconds panel for the handover."

## demo: place a food-delivery order (app/delivery.WorkflowType) and poll it
## to completion, printing status on every check. order_id is timestamped so
## this can be run repeatedly without colliding with an existing execution.
##
## Any of the three nodes can be asked, but only the shard's owner accepts
## the request — a node that is not the owner answers 409 NotOwner with the
## real owner's name in the body's current_owner field (see
## internal/api/errors.go). This target follows that redirect itself, rather
## than relying on request forwarding another agent may or may not have
## landed: on a 409 it reads current_owner and retries against that node's
## own published port (node-a/b/c -> 8081/8082/8083). That is also why a
## plain `curl -sf` cannot be used here — -f turns the 409 into a bare exit
## 22 with the body already discarded, which is the failure this replaces.
demo:
	@node_port() { case "$$1" in \
		node-a) echo 8081 ;; \
		node-b) echo 8082 ;; \
		node-c) echo 8083 ;; \
		*) echo "" ;; \
	esac; }; \
	request() { \
		method="$$1"; path="$$2"; data="$$3"; \
		tries=0; \
		while :; do \
			tries=$$((tries + 1)); \
			body_file="$$(mktemp)"; \
			if [ -n "$$data" ]; then \
				code="$$(curl -s -o "$$body_file" -w '%{http_code}' -X "$$method" "$$url$$path" -H 'Content-Type: application/json' -d "$$data")"; \
			else \
				code="$$(curl -s -o "$$body_file" -w '%{http_code}' -X "$$method" "$$url$$path")"; \
			fi; \
			resp_body="$$(cat "$$body_file")"; rm -f "$$body_file"; \
			if [ "$$code" = "409" ] && [ "$$tries" -lt 10 ]; then \
				owner="$$(echo "$$resp_body" | jq -r '.current_owner // empty')"; \
				new_port=""; \
				[ -n "$$owner" ] && [ "$$owner" != "null" ] && new_port="$$(node_port "$$owner")"; \
				if [ -n "$$new_port" ]; then \
					echo "demo: $$url$$path is not the owner (current_owner=$$owner) — retrying via port $$new_port" >&2; \
					url="http://localhost:$$new_port"; \
				else \
					echo "demo: got 409 with no usable current_owner yet — retrying $$url$$path in 2s" >&2; \
					sleep 2; \
				fi; \
				continue; \
			fi; \
			if [ "$$code" -lt 200 ] || [ "$$code" -ge 300 ]; then \
				echo "demo: $$method $$path failed with HTTP $$code: $$resp_body" >&2; \
				exit 1; \
			fi; \
			RESP_BODY="$$resp_body"; \
			return 0; \
		done; \
	}; \
	url="$(BASE_URL)"; \
	order_id="demo-order-$$(date +%s)"; \
	echo "demo: starting DeliveryOrder $$order_id"; \
	payload="{\"workflow_id\":\"$$order_id\",\"workflow_type\":\"DeliveryOrder\",\"input\":{\"order_id\":\"$$order_id\",\"customer_id\":\"cust-demo\",\"restaurant_id\":\"rest-demo\",\"items\":[{\"sku\":\"SKU-42\",\"quantity\":2}],\"amount_cents\":2599,\"currency\":\"USD\",\"prep_time_ns\":3000000000}}"; \
	request POST /v1/workflows "$$payload"; \
	echo "$$RESP_BODY" | jq .; \
	echo "demo: polling status (30 attempts, 2s apart)"; \
	for i in $$(seq 1 30); do \
		request GET /v1/workflows/$$order_id ""; \
		status="$$(echo "$$RESP_BODY" | jq -r '.status')"; \
		echo "demo:   [$$i/30] $$order_id status=$$status"; \
		case "$$status" in \
			COMPLETED|FAILED|COMPENSATING_FAILED) echo "demo: reached terminal state $$status"; exit 0 ;; \
		esac; \
		sleep 2; \
	done; \
	echo "demo: did not reach a terminal state in 60s — check \`make logs\`"; \
	exit 1

## verify: run the duplicate-event safety assertion against DATABASE_URL.
## Fails the build (non-zero exit) if any (shard_id, workflow_id, run_id,
## seq) tuple was ever written more than once.
verify:
	DATABASE_URL="$(DATABASE_URL)" ./deploy/assert_no_duplicates.sh

## check-replication: prove the standby is actually replicating rather than
## merely reporting "healthy" — writes a sentinel row on the leader and
## confirms it shows up on the standby within a few seconds, failing loudly
## if it never does. This is what makes the FR-9 claim demonstrable.
check-replication:
	LEADER_URL="$(DATABASE_URL)" STANDBY_URL="$(STANDBY_DATABASE_URL)" ./deploy/check_replication.sh

## promote-drill: kill the leader under load, promote the standby, repoint the
## three nodes at it and measure promotion time, data lost, workflows lost and
## manual steps (see deploy/promote_standby.sh). Destructive to the running
## cluster by design: it restores leader + standby + nodes afterwards, which
## discards whatever the promoted standby wrote. RESTORE=0 skips that.
## Record the output under docs/results/ with `make promote-drill 2>&1 | tee`.
promote-drill:
	RESTORE="$(RESTORE)" ./deploy/promote_standby.sh

## embedder-logs: follow the embedding service's logs on their own. `make
## logs` interleaves all nine services, and the model-loading messages
## (which take tens of seconds and matter when something is slow to become
## healthy) are easy to lose in that combined stream.
embedder-logs:
	$(COMPOSE) logs -f --tail=200 embedder

## experiment: run the precision/recall/latency harness (cmd/experiment)
## against the real embedding service running in this stack, instead of the
## built-in FakeEmbedder — this is what turns the semantic-dedup claim into
## a measured number rather than "the plumbing works." Requires the embedder
## service (`make up`, or at least `docker compose -f $(COMPOSE_FILE) up -d
## --wait embedder`) to already be healthy.
experiment:
	go run ./cmd/experiment -embedder=http -endpoint=$(EXPERIMENT_ENDPOINT)
