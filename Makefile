REGISTRY     ?= ghcr.io/ray
VERSION      ?= latest
TELEMETRY_IMG = $(REGISTRY)/gpu-vram-telemetry-daemon:$(VERSION)
SCHEDULER_IMG = $(REGISTRY)/gpu-packer-scheduler:$(VERSION)

.PHONY: all deps test build build-telemetry build-scheduler \
        docker-build docker-build-telemetry docker-build-scheduler \
        deploy undeploy fmt lint help

## ── Default ────────────────────────────────────────────────────────────────

all: deps test build

## ── Dependencies ───────────────────────────────────────────────────────────

# Resolve all Go module dependencies (required once after cloning because the
# k8s.io/kubernetes staging-repo replace directives need indirect deps resolved).
deps:
	go mod tidy

## ── Quality ────────────────────────────────────────────────────────────────

test:
	go test ./internal/... -v -race -count=1

lint:
	golangci-lint run ./...

fmt:
	gofmt -w ./...

## ── Build ──────────────────────────────────────────────────────────────────

build: build-telemetry build-scheduler

build-telemetry:
	CGO_ENABLED=0 go build -trimpath -o bin/telemetry-daemon ./cmd/telemetry-daemon

build-scheduler:
	CGO_ENABLED=0 go build -trimpath -o bin/scheduler ./cmd/scheduler

## ── Docker ─────────────────────────────────────────────────────────────────

docker-build: docker-build-telemetry docker-build-scheduler

docker-build-telemetry:
	docker build -f Dockerfile.telemetry -t $(TELEMETRY_IMG) .

docker-build-scheduler:
	docker build -f Dockerfile.scheduler -t $(SCHEDULER_IMG) .

docker-push: docker-build
	docker push $(TELEMETRY_IMG)
	docker push $(SCHEDULER_IMG)

## ── Kubernetes ─────────────────────────────────────────────────────────────

deploy:
	kubectl apply -f deploy/namespace.yaml
	kubectl apply -f deploy/telemetry-daemon/
	kubectl apply -f deploy/scheduler/rbac.yaml
	kubectl apply -f deploy/scheduler/scheduler-config.yaml
	kubectl apply -f deploy/scheduler/deployment.yaml
	kubectl apply -f deploy/observability/

undeploy:
	kubectl delete -f deploy/observability/ --ignore-not-found
	kubectl delete -f deploy/scheduler/ --ignore-not-found
	kubectl delete -f deploy/telemetry-daemon/ --ignore-not-found
	kubectl delete -f deploy/namespace.yaml --ignore-not-found

## ── Local Telemetry (run daemon locally for quick testing) ─────────────────

run-telemetry: build-telemetry
	NODE_NAME=sim-node-h100 \
	VRAM_TOTAL_BYTES=80000000000 \
	VRAM_AVAILABLE_BYTES=72000000000 \
	VRAM_FRAGMENTATION_RATIO=0.08 \
	LISTEN_ADDR=:8080 \
	./bin/telemetry-daemon

## ── Help ───────────────────────────────────────────────────────────────────

help:
	@grep -E '^[a-zA-Z_-]+:' Makefile | awk -F: '{printf "  %-25s\n", $$1}'
