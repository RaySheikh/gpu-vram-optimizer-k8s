package telemetry

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// SimulatedNode defines the initial VRAM state for a single emulated GPU node.
type SimulatedNode struct {
	NodeName           string
	VRAMTotalBytes     int64
	VRAMAvailableBytes int64
	FragmentationRatio float64
}

// DefaultSimulatedNodes returns the default two-node simulation cluster described
// in Phase 2 of the design: an 80 GB H100 node and a 24 GB A10G node.
func DefaultSimulatedNodes() []SimulatedNode {
	return []SimulatedNode{
		{
			NodeName:           "sim-node-h100",
			VRAMTotalBytes:     80_000_000_000,
			VRAMAvailableBytes: 72_000_000_000, // ~90% free
			FragmentationRatio: 0.08,
		},
		{
			NodeName:           "sim-node-a10g",
			VRAMTotalBytes:     24_000_000_000,
			VRAMAvailableBytes: 20_000_000_000, // ~83% free
			FragmentationRatio: 0.25,
		},
	}
}

// Daemon is the Telemetry Emulation Daemon (Component A).
// It exposes VRAM metrics for simulated GPU nodes via both:
//   - /metrics       → Prometheus text format (scraped by Prometheus server)
//   - /api/v1/nodes  → JSON API (polled directly by the scheduler plugin)
type Daemon struct {
	mu         sync.RWMutex
	nodes      map[string]*NodeMetrics
	listenAddr string
}

// NewDaemon creates a Daemon populated from the provided simulated nodes.
func NewDaemon(nodes []SimulatedNode, listenAddr string) *Daemon {
	d := &Daemon{
		nodes:      make(map[string]*NodeMetrics, len(nodes)),
		listenAddr: listenAddr,
	}
	for _, n := range nodes {
		n := n // capture range var
		d.nodes[n.NodeName] = &NodeMetrics{
			NodeName:           n.NodeName,
			VRAMTotalBytes:     n.VRAMTotalBytes,
			VRAMAvailableBytes: n.VRAMAvailableBytes,
			FragmentationRatio: n.FragmentationRatio,
		}
	}
	return d
}

// NewDaemonFromEnv reads NODE_NAME, VRAM_TOTAL_BYTES, VRAM_AVAILABLE_BYTES,
// VRAM_FRAGMENTATION_RATIO, and LISTEN_ADDR from the environment to build a
// single-node daemon suitable for DaemonSet deployments.
func NewDaemonFromEnv() *Daemon {
	node := SimulatedNode{
		NodeName:           envOrDefault("NODE_NAME", "sim-node-0"),
		VRAMTotalBytes:     envInt64("VRAM_TOTAL_BYTES", 80_000_000_000),
		VRAMAvailableBytes: envInt64("VRAM_AVAILABLE_BYTES", 72_000_000_000),
		FragmentationRatio: envFloat64("VRAM_FRAGMENTATION_RATIO", 0.1),
	}
	listenAddr := envOrDefault("LISTEN_ADDR", ":8080")
	return NewDaemon([]SimulatedNode{node}, listenAddr)
}

// Run registers all HTTP handlers and starts the HTTP server. Blocks until error.
func (d *Daemon) Run() error {
	reg := prometheus.NewRegistry()
	vramAvailable := promauto.With(reg).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nvidia_gpu_vram_available_bytes",
			Help: "Available VRAM on the emulated GPU node in bytes.",
		},
		[]string{"node"},
	)
	fragRatio := promauto.With(reg).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nvidia_gpu_vram_fragmentation_ratio",
			Help: "VRAM fragmentation ratio (0.0 = contiguous, 1.0 = fully fragmented).",
		},
		[]string{"node"},
	)

	d.mu.RLock()
	for _, m := range d.nodes {
		vramAvailable.WithLabelValues(m.NodeName).Set(float64(m.VRAMAvailableBytes))
		fragRatio.WithLabelValues(m.NodeName).Set(m.FragmentationRatio)
	}
	d.mu.RUnlock()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", d.handleHealthz)
	mux.HandleFunc("/api/v1/nodes", d.handleAllNodes)
	mux.HandleFunc("/api/v1/nodes/", d.handleNodeByName)

	fmt.Printf("Telemetry daemon listening on %s\n", d.listenAddr)
	return http.ListenAndServe(d.listenAddr, mux)
}

func (d *Daemon) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

// handleAllNodes returns JSON for all simulated nodes.
// GET /api/v1/nodes
func (d *Daemon) handleAllNodes(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	list := make([]*NodeMetrics, 0, len(d.nodes))
	for _, m := range d.nodes {
		list = append(list, m)
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(list); err != nil {
		log.Printf("encode node list: %v", err)
	}
}

// handleNodeByName returns JSON for a specific node.
// GET /api/v1/nodes/{nodeName}
func (d *Daemon) handleNodeByName(w http.ResponseWriter, r *http.Request) {
	nodeName := r.URL.Path[len("/api/v1/nodes/"):]
	if nodeName == "" {
		http.Error(w, "node name required", http.StatusBadRequest)
		return
	}

	d.mu.RLock()
	m, ok := d.nodes[nodeName]
	d.mu.RUnlock()

	if !ok {
		http.Error(w, fmt.Sprintf("node %q not found", nodeName), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(m); err != nil {
		log.Printf("encode node %q: %v", nodeName, err)
	}
}

// --- helpers ---

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		i, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			return i
		}
	}
	return def
}

func envFloat64(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			return f
		}
	}
	return def
}
