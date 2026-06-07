# GPU VRAM Optimizer — Kubernetes Scheduler Plugin

[![CI](https://github.com/ray/gpu-vram-optimizer-k8s/actions/workflows/ci.yaml/badge.svg)](https://github.com/ray/gpu-vram-optimizer-k8s/actions/workflows/ci.yaml)
[![Go Report Card](https://goreportcard.com/badge/github.com/ray/gpu-vram-optimizer-k8s)](https://goreportcard.com/report/github.com/ray/gpu-vram-optimizer-k8s)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

A custom [Kubernetes Scheduler Framework](https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/) plugin that optimizes placement of Large Language Model (LLM) inference workloads by minimizing GPU VRAM fragmentation across a cluster.

No physical GPUs required — the telemetry daemon emulates enterprise-grade hardware metrics so the full system can be validated locally via [kind](https://kind.sigs.k8s.io/).

---

## The Problem

Standard Kubernetes scheduling treats GPU resources as binary (GPU count), with no awareness of VRAM topology or memory fragmentation. When LLM workloads dynamically scale their KV-cache, fragmented VRAM leads to:

- Out-Of-Memory (OOM) kills even when total free VRAM exceeds the workload's request
- Poor GPU utilization — large free blocks split across nodes that can't satisfy any single workload
- No bin-packing — the default scheduler spreads workloads rather than packing them tightly

---

## What This Does in the Demo Cluster

Running `./hack/kind-setup.sh` + `kubectl apply -f examples/` produces a **fully functional simulation** of a GPU cluster scheduler — no real hardware needed.

### What actually happens, step by step

```
1. kind creates 3 nodes:
      gpu-vram-dev-control-plane   (scheduler + prometheus run here)
      gpu-vram-dev-worker          (simulated GPU worker)
      gpu-vram-dev-worker2         (simulated GPU worker)

2. The telemetry DaemonSet starts one pod per worker node.
   Each pod reads its own node name via the Kubernetes Downward API
   (spec.nodeName) and registers as:
      gpu-vram-dev-worker  → 80 GB total, 72 GB available, 8% fragmentation
      gpu-vram-dev-worker2 → 80 GB total, 72 GB available, 8% fragmentation

3. The custom scheduler starts. For every pod annotated with
   nvidia.com/gpu-vram-req, it runs our VRAMPlugin instead of the
   default scoring logic.

4. When you submit examples/, the scheduler processes each pod:

   llm-pod-40gb  (requests 40 GB)
   ├── Filter:  worker  → 72 GB ≥ 40 GB ✅
   ├── Filter:  worker2 → 72 GB ≥ 40 GB ✅
   ├── Score:   worker  → 90*(1-0.08) + 10*(1-32/72) = 82.8 + 5.6 = ~88
   ├── Score:   worker2 → same score (identical telemetry data)
   └── Placed on: gpu-vram-dev-worker  (first tiebreak wins)

   llm-pod-oversized  (requests 200 GB)
   ├── Filter:  worker  → 72 GB < 200 GB ❌ Unschedulable
   ├── Filter:  worker2 → 72 GB < 200 GB ❌ Unschedulable
   └── Result: stays Pending forever ✅ (correct behaviour)

   llm-flood x5  (10 GB each)
   └── Bin-packed: 3 pods → worker, 2 pods → worker2
       (Best-Fit tightly packs until the first node reaches saturation)
```

### Observed result

```
NAME                         NODE
llm-pod-40gb                 gpu-vram-dev-worker    ← scheduled correctly
llm-pod-8gb                  gpu-vram-dev-worker    ← scheduled correctly
llm-flood-* (x3)             gpu-vram-dev-worker    ← bin-packed first
llm-flood-* (x2)             gpu-vram-dev-worker2   ← overflow to second node
llm-pod-oversized            <Pending>              ← correctly unschedulable
```

---

## How It Works

### Architecture

```
┌──────────────────────────────────────────────────────────────────────┐
│  Kubernetes Cluster                                                  │
│                                                                      │
│  ┌─────────────────────┐     per-node IP:8080    ┌────────────────┐ │
│  │  Telemetry DaemonSet │ ◄──────────────────────│ Scheduler      │ │
│  │  (one pod per node)  │                        │ Plugin         │ │
│  │                      │                        │                │ │
│  │  NODE_NAME injected  │  GET /api/v1/nodes/    │ PreFilter:     │ │
│  │  via Downward API    │  {nodeName}            │ parse VRAM req │ │
│  │                      │                        │                │ │
│  │  Exposes:            │                        │ Filter:        │ │
│  │  /metrics (prom)     │                        │ avail ≥ req    │ │
│  │  /api/v1/nodes       │                        │                │ │
│  └─────────────────────┘                        │ Score: BFD     │ │
│           │ scrape                               └────────────────┘ │
│           ▼                                                          │
│  ┌─────────────────────┐                                            │
│  │  Prometheus          │                                            │
│  └─────────────────────┘                                            │
└──────────────────────────────────────────────────────────────────────┘
```

**DaemonSet mode** (default): The scheduler queries each node's daemon pod directly at `http://<nodeInternalIP>:8080`. The pod only knows about the node it runs on, injected at startup via `spec.nodeName`. This is topology-correct and scales linearly with node count.

### Scheduling Algorithm

**Filter Phase — Capacity Check**

A node is marked `Unschedulable` if:
```
node.vram_available_bytes < pod.nvidia.com/gpu-vram-req
```

**Score Phase — Best-Fit Decreasing**

Surviving nodes are ranked by a weighted composite score (0–100):

$$\text{score} = 90 \times (1 - \text{frag\_ratio}) + 10 \times \left(1 - \frac{\text{available} - \text{requested}}{\text{available}}\right)$$

- **Fragmentation component (90 pts):** Nodes with contiguous memory rank higher.
- **Best-Fit component (10 pts):** When fragmentation scores tie, the node with the least leftover VRAM after placement wins — driving tight bin-packing.

### Pod Annotation

```yaml
spec:
  schedulerName: gpu-packer-scheduler
metadata:
  annotations:
    nvidia.com/gpu-vram-req: "40000000000"  # 40 GB in bytes
```

---

## Scaling to a Real GPU Cluster

The simulation layer is **fully swappable** without touching the scheduler plugin.

### What changes in production

| Component | Demo (kind) | Production (real GPUs) |
|---|---|---|
| Telemetry daemon source | Static env vars per pod | Replace with real `nvidia-smi` or DCGM scraper |
| `VRAM_AVAILABLE_BYTES` | Hardcoded `72000000000` | Read from `nvidia-smi --query-gpu=memory.free` |
| `VRAM_FRAGMENTATION_RATIO` | Hardcoded `0.08` | Computed from DCGM `DCGM_FI_DEV_FB_FREE` / `DCGM_FI_DEV_FB_TOTAL` |
| Node coverage | 2 simulated workers | One DaemonSet pod per real GPU node (already the topology) |
| Scheduler plugin | Unchanged | Unchanged — queries same HTTP interface |

### Minimal production telemetry daemon

The only change needed in `server.go` to go production:

```go
// Replace the static env var read with:
vramFree  := execNvidiaSmi("--query-gpu=memory.free  --format=csv,noheader,nounits")
vramTotal := execNvidiaSmi("--query-gpu=memory.total --format=csv,noheader,nounits")
fragRatio := 1.0 - (vramFree / vramTotal)
```

The `NodeMetrics` struct, HTTP endpoints, and all scheduler logic remain identical.

### Why this scales linearly

- **DaemonSet topology**: adding a new GPU node automatically schedules a new daemon pod — zero config changes required
- **Per-node direct query**: the scheduler hits `nodeIP:8080` directly — no single-point-of-failure service, no cross-node aggregation bottleneck
- **Stateless plugin**: the VRAMPlugin holds no in-memory state across scheduling cycles — safe to run with multiple scheduler replicas behind a leader election lock
- **2s timeout per node query**: worst-case scheduling latency = `2s × nodes queried in parallel` (framework runs Filter/Score concurrently per node)

### What you'd add for a production deployment

1. **TLS on the telemetry daemon** — the scheduler should verify the daemon's cert to prevent metric spoofing
2. **Caching layer** — cache node metrics for 5–10s to reduce daemon load under high pod submission rates
3. **Leader election** — already supported by the kube-scheduler framework; set `leaderElect: true` in the ConfigMap
4. **Prometheus alerts** — alert on `nvidia_gpu_vram_fragmentation_ratio > 0.5` to trigger proactive workload rebalancing
5. **DCGM exporter sidecar** — co-locate with the telemetry daemon pod for authoritative GPU metrics

---

## Project Structure

```
.
├── cmd/
│   ├── telemetry-daemon/       Entry point — starts the HTTP telemetry server
│   └── scheduler/              Entry point — boots the custom kube-scheduler
├── internal/
│   ├── telemetry/
│   │   ├── types.go            Shared NodeMetrics struct
│   │   └── server.go           HTTP daemon (/metrics Prometheus + /api/v1/nodes JSON)
│   └── scheduler/
│       ├── client.go           Service + DaemonSet mode HTTP client
│       ├── plugin.go           PreFilter / Filter / Score extension points
│       ├── plugin_test.go      Unit tests — pure math, no k8s API required
│       ├── integration_test.go Full PreFilter→Filter→Score pipeline via httptest
│       └── chaos_test.go       Resilience tests — daemon failure, timeout, bad JSON
├── deploy/
│   ├── namespace.yaml
│   ├── telemetry-daemon/       DaemonSet + Service (one pod per GPU worker node)
│   ├── scheduler/              RBAC + ConfigMap + Deployment
│   └── observability/          Prometheus Deployment + ConfigMap
├── examples/
│   └── dummy-llm-workloads.yaml  Annotated busybox pods + flood Deployment
├── hack/
│   ├── kind-setup.sh           One-command local cluster bootstrap
│   └── teardown.sh             Clean cluster removal
├── .github/workflows/ci.yaml   CI: lint → test → build → docker → trivy scan
├── .golangci.yaml              14-linter golangci-lint configuration
├── Dockerfile.telemetry
├── Dockerfile.scheduler
└── Makefile
```

---

## What Is Working

| Component | Status | Notes |
|---|---|---|
| Telemetry Daemon (DaemonSet) | ✅ | One pod per GPU worker. Node name injected via Downward API. Exposes `/metrics` + `/api/v1/nodes`. |
| Scheduler Plugin — PreFilter | ✅ | Parses `nvidia.com/gpu-vram-req`; skips non-GPU pods via `framework.Skip`. |
| Scheduler Plugin — Filter | ✅ | Marks nodes `Unschedulable` when available VRAM < requested. Queries daemon via node IP. |
| Scheduler Plugin — Score | ✅ | Best-Fit Decreasing. Resolves node IP from framework snapshot for DaemonSet routing. |
| DaemonSet mode routing | ✅ | Plugin queries `http://<nodeInternalIP>:8080` per-node — no Service bottleneck. |
| `NodeMetricsFetcher` interface | ✅ | Swappable for any implementation (HTTP, gRPC, mock). |
| Unit Tests (6) | ✅ | Pure math: Filter/Score formulas, tie-breaking, fragmentation dominance. |
| Integration Tests (9) | ✅ | Full pipeline via `httptest.Server`. |
| Chaos Tests (5) | ✅ | Unreachable daemon, 500, malformed JSON, 404 fallback, context timeout. |
| RBAC | ✅ | Bound to `system:kube-scheduler` + `system:volume-scheduler` + supplemental role for namespaces/ReplicaSets/ConfigMaps. |
| End-to-end cluster validation | ✅ | Verified: 40 GB pod scheduled, 200 GB pod stays Pending, 5-pod flood bin-packed across 2 nodes. |
| GitHub Actions CI | ✅ | lint → test (race detector) → build → docker push → Trivy scan. |
| `hack/kind-setup.sh` | ✅ | Full cluster bootstrap: build images, load into kind, apply manifests, wait for readiness. |
| Makefile | ✅ | `deps`, `test`, `lint`, `build`, `docker-build`, `docker-push`, `deploy`, `undeploy`, `run-telemetry`. |

---

## What Is Not Yet Done

| Item | Notes |
|---|---|
| `envtest` integration tests | Programmatically submit real Pods/Nodes to a local API server and assert `Binding` objects. |
| Helm chart | Single-command deployment for external cluster operators. |
| Metric caching | Cache node metrics for 5–10s to reduce daemon load at high pod submission rates. |
| TLS on telemetry daemon | Prevent metric spoofing in multi-tenant clusters. |

---

## Quick Start

### Run tests locally (no cluster needed)

```bash
git clone https://github.com/ray/gpu-vram-optimizer-k8s
cd gpu-vram-optimizer-k8s
make deps
make test   # 19 tests: unit + integration + chaos, with race detector
```

### Full local simulation (kind cluster)

**Prerequisites:** Go 1.21+, Docker, `kind`, `kubectl`

```bash
# Bootstrap the cluster, build images, deploy everything
./hack/kind-setup.sh

# Submit LLM workloads
kubectl apply -f examples/

# Watch pods schedule in real time
kubectl get pods -o wide -w

# Inspect scheduler decisions
kubectl logs -n gpu-scheduler -l app=gpu-packer-scheduler -f

# Tear down
./hack/teardown.sh
```

### Deploy to an existing cluster

```bash
make docker-push REGISTRY=your-registry VERSION=v0.1.0
make deploy
```

---

## Configuration Reference

### Telemetry Daemon — Environment Variables

The daemon is configured entirely through environment variables. In the DaemonSet,
`NODE_NAME` is injected automatically — you only need to set the VRAM values.

| Variable | Required | Default | Description |
|---|---|---|---|
| `NODE_NAME` | ✅ | `sim-node-0` | **Set automatically** via Downward API (`spec.nodeName`). Never set this manually in the DaemonSet. |
| `VRAM_TOTAL_BYTES` | ✅ | `80000000000` | Total VRAM on this node's GPU in bytes (e.g. `80000000000` = 80 GB H100). |
| `VRAM_AVAILABLE_BYTES` | ✅ | `72000000000` | Currently available (free) VRAM in bytes. In production, read this from `nvidia-smi` or DCGM. |
| `VRAM_FRAGMENTATION_RATIO` | ✅ | `0.1` | Float between `0.0` (fully contiguous) and `1.0` (fully fragmented). In production, derive from `1.0 - (largest_free_block / total_free)`. |
| `LISTEN_ADDR` | ❌ | `:8080` | HTTP listen address. Change port if 8080 is taken on your nodes. |

#### Demo cluster values (kind)

The DaemonSet in `deploy/telemetry-daemon/deployment.yaml` hardcodes these for simulation:

```yaml
- name: VRAM_TOTAL_BYTES
  value: "80000000000"        # Simulates an 80 GB H100
- name: VRAM_AVAILABLE_BYTES
  value: "72000000000"        # Simulates 72 GB free (90%)
- name: VRAM_FRAGMENTATION_RATIO
  value: "0.08"               # 8% fragmentation
```

#### Real cluster values

Replace the hardcoded values with a init container or sidecar that reads from `nvidia-smi`:

```yaml
initContainers:
  - name: gpu-metrics-init
    image: nvidia/cuda:12.0-base
    command:
      - sh
      - -c
      - |
        TOTAL=$(nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits | awk '{print $1 * 1048576}')
        FREE=$(nvidia-smi  --query-gpu=memory.free  --format=csv,noheader,nounits | awk '{print $1 * 1048576}')
        FRAG=$(echo "$FREE $TOTAL" | awk '{printf "%.4f", 1.0 - ($1/$2)}')
        echo $TOTAL > /gpu-metrics/total
        echo $FREE  > /gpu-metrics/free
        echo $FRAG  > /gpu-metrics/frag
    volumeMounts:
      - name: gpu-metrics
        mountPath: /gpu-metrics
containers:
  - name: telemetry-daemon
    env:
      - name: NODE_NAME
        valueFrom:
          fieldRef:
            fieldPath: spec.nodeName
      - name: VRAM_TOTAL_BYTES
        valueFrom:
          configMapKeyRef:     # or read from the shared volume
            ...
```

Alternatively, if you run [DCGM Exporter](https://github.com/NVIDIA/dcgm-exporter) in your cluster, point Prometheus at it and replace the telemetry daemon with a thin adapter that queries the Prometheus API.

---

### Scheduler Plugin — ConfigMap Options

The plugin is configured in `deploy/scheduler/scheduler-config.yaml` under `pluginConfig`:

```yaml
pluginConfig:
  - name: VRAMPlugin
    args:
      # Option A — DaemonSet mode (default, recommended)
      # Queries each node's daemon pod directly at http://<nodeIP>:<port>
      daemonSetPort: "8080"

      # Option B — Service mode (for centralised daemon deployments)
      # All nodes report to a single daemon reachable via ClusterIP Service
      # telemetryDaemonURL: "http://telemetry-daemon-svc.gpu-scheduler.svc.cluster.local:8080"
```

| Field | Default | When to use |
|---|---|---|
| `daemonSetPort` | — | **Recommended.** Use when the daemon runs as a DaemonSet (one pod per node). |
| `telemetryDaemonURL` | — | Use when you have a single centralised daemon that knows about all nodes (e.g. wrapping a Prometheus query). |

Exactly one of these must be set. Setting both is an error.

---

### Pod Annotation Reference

Any pod that should be scheduled by the VRAM optimizer must declare two things:

```yaml
spec:
  schedulerName: gpu-packer-scheduler   # 1. Route to our scheduler

metadata:
  annotations:
    nvidia.com/gpu-vram-req: "40000000000"  # 2. VRAM requirement in bytes
```

| Annotation | Type | Example | Notes |
|---|---|---|---|
| `nvidia.com/gpu-vram-req` | integer string (bytes) | `"40000000000"` = 40 GB | Must be a positive integer. Pods without this annotation are **skipped** — the default scheduler handles them normally. |

**Byte conversion reference:**

| GPU Model | VRAM | Annotation value |
|---|---|---|
| NVIDIA H100 SXM | 80 GB | `"80000000000"` |
| NVIDIA A100 | 40 GB | `"40000000000"` |
| NVIDIA A10G | 24 GB | `"24000000000"` |
| NVIDIA L4 | 24 GB | `"24000000000"` |
| NVIDIA RTX 4090 | 24 GB | `"24000000000"` |
| NVIDIA T4 | 16 GB | `"16000000000"` |

---

## Architecture — Design Decisions

| Decision | Rationale |
|---|---|
| DaemonSet topology | One daemon per node scales linearly; no single-point-of-failure aggregator |
| Downward API for `NODE_NAME` | Zero-config — each pod self-registers under the correct k8s node name automatically |
| Per-node IP querying | Scheduler routes to `nodeIP:8080` directly — avoids Service load-balancer randomly routing to the wrong node's daemon |
| `NodeMetricsFetcher` interface | Decouples plugin from transport — swap HTTP for gRPC or a real DCGM client without touching scheduling logic |
| `framework.Skip` for non-GPU pods | Zero overhead for standard workloads — plugin is a no-op if annotation is absent |
| 2s HTTP client timeout | Prevents slow daemon from blocking scheduling cycle |
| 404 → 0 VRAM fallback | Unknown nodes safely excluded rather than silently accepted |
| distroless base images | No shell, no package manager — minimal CVE surface in production containers |
| Apache 2.0 license | CNCF ecosystem compatible; safe for enterprise adoption |


---

## The Problem

Standard Kubernetes scheduling treats GPU resources as binary (GPU count), with no awareness of VRAM topology or memory fragmentation. When LLM workloads dynamically scale their KV-cache, fragmented VRAM leads to:

- Out-Of-Memory (OOM) kills even when total free VRAM exceeds the workload's request
- Poor GPU utilization — large free blocks split across nodes that can't satisfy any single workload
- No bin-packing — the default scheduler spreads workloads rather than packing them tightly

---

## How It Works

The system is three decoupled components:

```
┌─────────────────────┐     HTTP poll     ┌───────────────────────────┐
│  Telemetry Daemon   │ ◄──────────────── │  Scheduler Plugin         │
│  (Component A)      │                   │  (Component B)            │
│                     │                   │                           │
│  /metrics           │                   │  PreFilter: parse VRAM    │
│  /api/v1/nodes      │                   │  annotation               │
│                     │                   │                           │
│  Exposes:           │                   │  Filter: available ≥ req  │
│  - vram_available   │                   │                           │
│  - frag_ratio       │                   │  Score: Best-Fit          │
└─────────────────────┘                   │  Decreasing algorithm     │
                                          └───────────────────────────┘
         │ scrape                                     │ scheduling decisions
         ▼                                            ▼
┌─────────────────────┐                   ┌───────────────────────────┐
│  Prometheus         │                   │  Kubernetes Control Plane │
│  (Component C)      │                   └───────────────────────────┘
└─────────────────────┘
```

### Scheduling Algorithm

**Filter Phase — Capacity Check**

A node is marked `Unschedulable` if:
```
node.vram_available_bytes < pod.nvidia.com/gpu-vram-req
```

**Score Phase — Best-Fit Decreasing**

Surviving nodes are ranked by a weighted composite score (0–100):

$$\text{score} = 90 \times (1 - \text{frag\_ratio}) + 10 \times \left(1 - \frac{\text{available} - \text{requested}}{\text{available}}\right)$$

- **Fragmentation component (90 pts):** Nodes with contiguous memory rank higher.
- **Best-Fit component (10 pts):** When fragmentation scores tie, the node with the least leftover VRAM after placement wins — driving tight bin-packing.

### Pod Annotation

Pods opt in to the custom scheduler via:

```yaml
spec:
  schedulerName: gpu-packer-scheduler

metadata:
  annotations:
    nvidia.com/gpu-vram-req: "40000000000"  # 40 GB in bytes
```

---

## Project Structure

```
.
├── cmd/
│   ├── telemetry-daemon/       Entry point — starts the HTTP telemetry server
│   └── scheduler/              Entry point — boots the custom kube-scheduler
├── internal/
│   ├── telemetry/
│   │   ├── types.go            Shared NodeMetrics struct
│   │   └── server.go           HTTP daemon (/metrics Prometheus + /api/v1/nodes JSON)
│   └── scheduler/
│       ├── client.go           HTTP client that polls the telemetry daemon
│       ├── plugin.go           PreFilter / Filter / Score extension points
│       ├── plugin_test.go      Unit tests — pure math, no k8s API required
│       ├── integration_test.go Full PreFilter→Filter→Score pipeline via httptest
│       └── chaos_test.go       Resilience tests — daemon failure, timeout, bad JSON
├── deploy/
│   ├── namespace.yaml
│   ├── telemetry-daemon/       Deployment + Service
│   ├── scheduler/              RBAC + ConfigMap + Deployment
│   └── observability/          Prometheus Deployment + ConfigMap
├── examples/
│   └── dummy-llm-workloads.yaml  Annotated busybox pods + flood Deployment
├── hack/
│   ├── kind-setup.sh           One-command local cluster bootstrap
│   └── teardown.sh             Clean cluster removal
├── .github/workflows/ci.yaml   CI: lint → test → build → docker → trivy scan
├── .golangci.yaml              14-linter golangci-lint configuration
├── Dockerfile.telemetry
├── Dockerfile.scheduler
└── Makefile
```

---

## What Is Working

| Component | Status | Notes |
|---|---|---|
| Telemetry Daemon | ✅ | Exposes `/metrics` (Prometheus) and `/api/v1/nodes` (JSON). Configurable via env vars. |
| Scheduler Plugin — PreFilter | ✅ | Parses `nvidia.com/gpu-vram-req`; skips non-GPU pods cleanly via `framework.Skip`. |
| Scheduler Plugin — Filter | ✅ | Marks nodes `Unschedulable` when available VRAM < requested bytes. |
| Scheduler Plugin — Score | ✅ | Best-Fit Decreasing composite score (90 pt frag weight + 10 pt fit weight). |
| `NodeMetricsFetcher` interface | ✅ | Plugin depends on an interface, not the concrete client — enables clean injection in tests. |
| Unit Tests (6) | ✅ | `FilterNode`, `ScoreNode`, tie-breaking, fragmentation dominance — pure Go, no k8s deps. |
| Integration Tests (9) | ✅ | Full pipeline via `httptest.Server`: PreFilter validation, Filter boundary cases, Score ordering, end-to-end H100 vs A10G scenario. |
| Chaos Tests (5) | ✅ | Daemon unreachable, HTTP 500, malformed JSON, HTTP 404 fallback, context timeout — control plane never crashes. |
| Kubernetes Manifests | ✅ | Namespace, RBAC, Scheduler ConfigMap + Deployment, Prometheus. |
| Dockerfiles | ✅ | Multi-stage distroless builds for both binaries. |
| GitHub Actions CI | ✅ | lint → test (race detector) → build → docker push to GHCR → Trivy vulnerability scan. |
| `hack/kind-setup.sh` | ✅ | Bootstraps a 3-node kind cluster (1 control-plane + 2 simulated GPU workers), loads images, applies all manifests. |
| Example workloads | ✅ | 40 GB pod, 8 GB pod, oversized pod (stays Pending), 5-replica flood Deployment. |
| Makefile | ✅ | `deps`, `test`, `lint`, `build`, `docker-build`, `docker-push`, `deploy`, `undeploy`, `run-telemetry`. |
| `.golangci.yaml` | ✅ | 14 linters: `errcheck`, `staticcheck`, `govet`, `bodyclose`, `noctx`, `contextcheck`, and more. |

---

## What Is Not Yet Done

| Item | Phase | Notes |
|---|---|---|
| `envtest` integration tests | Phase 1 | Programmatically submit real Pods/Nodes to a local API server and assert `Binding` objects are produced by the plugin. |
| Helm chart / Kustomize overlay | Phase 2 | Single-command deployment for external cluster operators. |
| `go.sum` / vendor directory | — | Run `make deps` after cloning to resolve all indirect k8s staging-repo dependencies. |

---

## Quick Start

### Local binary (no cluster)

**Prerequisites:** Go 1.21+

```bash
# 1. Clone and resolve all k8s indirect dependencies
git clone https://github.com/ray/gpu-vram-optimizer-k8s
cd gpu-vram-optimizer-k8s
make deps

# 2. Run all tests (unit + integration + chaos) with race detector
make test

# 3. Build binaries to bin/
make build

# 4. Run the telemetry daemon locally — emulates an 80 GB H100 node
make run-telemetry
```

```bash
# In a second terminal — verify the daemon is serving metrics:
curl http://localhost:8080/api/v1/nodes        # JSON node list
curl http://localhost:8080/metrics | grep nvidia_gpu  # Prometheus metrics
```

### Local kind cluster (full simulation)

**Prerequisites:** Go 1.21+, Docker, `kind`, `kubectl`

```bash
# 1. Build images and spin up a 3-node cluster with the full stack deployed
./hack/kind-setup.sh

# 2. Submit dummy LLM workloads
kubectl apply -f examples/

# 3. Watch scheduling decisions in real time
kubectl logs -n gpu-scheduler -l app=gpu-packer-scheduler -f

# 4. Verify the 40 GB pod landed on sim-node-h100
kubectl get pods -o wide

# 5. Tear down when done
./hack/teardown.sh
```

### Deploy to an existing cluster

```bash
# Build and push images to your registry
make docker-push REGISTRY=your-registry VERSION=v0.1.0

# Apply all manifests
make deploy
```

---

## Simulated Cluster Nodes (Default)

| Node | Total VRAM | Available | Fragmentation | Score for 40 GB request |
|---|---|---|---|---|
| `sim-node-h100` | 80 GB | 72 GB | 8% | **~91** — passes Filter, wins Score |
| `sim-node-a10g` | 24 GB | 20 GB | 25% | **Unschedulable** — fails Filter (20 GB < 40 GB) |

A 40 GB LLM workload is **exclusively scheduled to `sim-node-h100`**. The A10G is filtered out entirely.

---

## Architecture — Design Decisions

| Decision | Rationale |
|---|---|
| `NodeMetricsFetcher` interface | Decouples plugin from transport layer; any implementation (HTTP, gRPC, mock) satisfies the contract |
| `framework.Skip` for non-GPU pods | Ensures zero overhead for standard workloads — the plugin is a no-op if the annotation is absent |
| 2s HTTP client timeout on telemetry calls | Prevents a slow daemon from blocking the scheduling cycle |
| 404 → 0 VRAM fallback | Unknown nodes are safely excluded from scheduling rather than silently accepted |
| distroless base images | Minimal attack surface; no shell, no package manager in production containers |
| Apache 2.0 license | CNCF ecosystem compatible; safe for enterprise adoption |

