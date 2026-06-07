package scheduler_test

// Integration tests exercise the full PreFilter → Filter → Score pipeline using
// a real *TelemetryClient pointed at an httptest.Server. No Kubernetes API server
// is required — only the pure Go framework types (CycleState, NodeInfo) are used.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"github.com/ray/gpu-vram-optimizer-k8s/internal/scheduler"
	"github.com/ray/gpu-vram-optimizer-k8s/internal/telemetry"
)

// ── Helpers ──────────────────────────────────────────────────────────────────

// newMockDaemon creates an httptest.Server that serves NodeMetrics from the
// provided map keyed by node name. Unknown nodes receive a 404, matching the
// production daemon behaviour.
func newMockDaemon(t *testing.T, nodes map[string]telemetry.NodeMetrics) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/nodes/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/nodes/")
		m, ok := nodes[name]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(m); err != nil {
			http.Error(w, "encode error", http.StatusInternalServerError)
		}
	})
	return httptest.NewServer(mux)
}

// newPlugin constructs a VRAMPlugin pointed at the given daemon URL.
// handle is nil because the plugin does not use it in Filter/Score paths.
func newPlugin(t *testing.T, daemonURL string) framework.Plugin {
	t.Helper()
	p, err := scheduler.New(context.Background(), &scheduler.PluginConfig{
		TelemetryDaemonURL: daemonURL,
	}, nil)
	if err != nil {
		t.Fatalf("scheduler.New: %v", err)
	}
	return p
}

// newPod returns a pod with the given VRAM annotation value (bytes as string).
// Pass an empty string to create a pod with no annotation.
func newPod(vramAnnotation string) *v1.Pod {
	annotations := map[string]string{}
	if vramAnnotation != "" {
		annotations[scheduler.VRAMAnnotation] = vramAnnotation
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "llm-workload",
			Namespace:   "default",
			Annotations: annotations,
		},
		Spec: v1.PodSpec{SchedulerName: "gpu-packer-scheduler"},
	}
}

// newNodeInfo returns a NodeInfo containing a Node with the given name.
func newNodeInfo(nodeName string) *framework.NodeInfo {
	ni := framework.NewNodeInfo()
	ni.SetNode(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
	})
	return ni
}

// ── PreFilter tests ──────────────────────────────────────────────────────────

// TestPreFilter_PodWithoutAnnotation_Skipped verifies that pods which do not
// carry the VRAM annotation are skipped (framework.Skip status), leaving the
// default scheduler to handle them normally.
func TestPreFilter_PodWithoutAnnotation_Skipped(t *testing.T) {
	srv := newMockDaemon(t, nil)
	defer srv.Close()
	p := newPlugin(t, srv.URL)

	_, status := p.(framework.PreFilterPlugin).PreFilter(
		context.Background(),
		framework.NewCycleState(),
		newPod(""), // no annotation
	)
	if status.Code() != framework.Skip {
		t.Errorf("expected Skip for pod without annotation, got %v", status)
	}
}

// TestPreFilter_InvalidAnnotation_Error verifies that a non-integer annotation
// value produces an Error status, protecting against misconfigured pods.
func TestPreFilter_InvalidAnnotation_Error(t *testing.T) {
	srv := newMockDaemon(t, nil)
	defer srv.Close()
	p := newPlugin(t, srv.URL)

	_, status := p.(framework.PreFilterPlugin).PreFilter(
		context.Background(),
		framework.NewCycleState(),
		newPod("not-a-number"),
	)
	if status.Code() != framework.Error {
		t.Errorf("expected Error for invalid annotation, got %v", status)
	}
}

// TestPreFilter_NegativeBytes_Error verifies that a zero or negative VRAM
// request is rejected at the PreFilter stage.
func TestPreFilter_NegativeBytes_Error(t *testing.T) {
	srv := newMockDaemon(t, nil)
	defer srv.Close()
	p := newPlugin(t, srv.URL)

	_, status := p.(framework.PreFilterPlugin).PreFilter(
		context.Background(),
		framework.NewCycleState(),
		newPod("-1"),
	)
	if status.Code() != framework.Error {
		t.Errorf("expected Error for negative bytes, got %v", status)
	}
}

// ── Filter tests ─────────────────────────────────────────────────────────────

// TestFilter_SufficientVRAM_Passes verifies the Filter phase passes a node
// whose available VRAM meets the pod's request.
func TestFilter_SufficientVRAM_Passes(t *testing.T) {
	srv := newMockDaemon(t, map[string]telemetry.NodeMetrics{
		"h100-node": {
			NodeName:           "h100-node",
			VRAMAvailableBytes: 72_000_000_000, // 72 GB
			FragmentationRatio: 0.08,
		},
	})
	defer srv.Close()
	p := newPlugin(t, srv.URL)

	const req = "40000000000" // 40 GB
	state := framework.NewCycleState()
	runPreFilter(t, p, state, req)

	status := p.(framework.FilterPlugin).Filter(
		context.Background(), state, newPod(req), newNodeInfo("h100-node"),
	)
	if !status.IsSuccess() {
		t.Errorf("expected node to pass Filter, got %v", status)
	}
}

// TestFilter_InsufficientVRAM_Unschedulable verifies the Filter phase rejects
// a node whose available VRAM is less than the pod's request.
func TestFilter_InsufficientVRAM_Unschedulable(t *testing.T) {
	srv := newMockDaemon(t, map[string]telemetry.NodeMetrics{
		"a10g-node": {
			NodeName:           "a10g-node",
			VRAMAvailableBytes: 20_000_000_000, // 20 GB
			FragmentationRatio: 0.25,
		},
	})
	defer srv.Close()
	p := newPlugin(t, srv.URL)

	const req = "40000000000" // 40 GB – won't fit
	state := framework.NewCycleState()
	runPreFilter(t, p, state, req)

	status := p.(framework.FilterPlugin).Filter(
		context.Background(), state, newPod(req), newNodeInfo("a10g-node"),
	)
	if status.Code() != framework.Unschedulable {
		t.Errorf("expected Unschedulable for node with insufficient VRAM, got %v", status)
	}
}

// TestFilter_ExactFit_Passes verifies that a node with exactly the requested
// bytes available is schedulable (boundary condition).
func TestFilter_ExactFit_Passes(t *testing.T) {
	const req = "8000000000" // 8 GB exactly
	srv := newMockDaemon(t, map[string]telemetry.NodeMetrics{
		"exact-node": {NodeName: "exact-node", VRAMAvailableBytes: 8_000_000_000},
	})
	defer srv.Close()
	p := newPlugin(t, srv.URL)

	state := framework.NewCycleState()
	runPreFilter(t, p, state, req)

	status := p.(framework.FilterPlugin).Filter(
		context.Background(), state, newPod(req), newNodeInfo("exact-node"),
	)
	if !status.IsSuccess() {
		t.Errorf("exact-fit node must be schedulable, got %v", status)
	}
}

// ── Score tests ───────────────────────────────────────────────────────────────

// TestScore_LowFragScoresHigher verifies that a node with lower fragmentation
// receives a higher score than one with higher fragmentation, when both have
// sufficient VRAM to accommodate the request.
func TestScore_LowFragScoresHigher(t *testing.T) {
	const req = "8000000000" // 8 GB
	srv := newMockDaemon(t, map[string]telemetry.NodeMetrics{
		"low-frag":  {NodeName: "low-frag", VRAMAvailableBytes: 72_000_000_000, FragmentationRatio: 0.05},
		"high-frag": {NodeName: "high-frag", VRAMAvailableBytes: 72_000_000_000, FragmentationRatio: 0.70},
	})
	defer srv.Close()
	p := newPlugin(t, srv.URL)

	state := framework.NewCycleState()
	runPreFilter(t, p, state, req)

	scoreLow, st := p.(framework.ScorePlugin).Score(context.Background(), state, newPod(req), "low-frag")
	if !st.IsSuccess() {
		t.Fatalf("scoring low-frag: %v", st)
	}
	scoreHigh, st := p.(framework.ScorePlugin).Score(context.Background(), state, newPod(req), "high-frag")
	if !st.IsSuccess() {
		t.Fatalf("scoring high-frag: %v", st)
	}
	if scoreLow <= scoreHigh {
		t.Errorf("low-frag node (score %d) should outscore high-frag node (score %d)", scoreLow, scoreHigh)
	}
}

// TestScore_TighterFitScoresHigherOnTie verifies that when fragmentation is
// equal, the node with less leftover VRAM after placement scores higher — the
// Best-Fit Decreasing tie-break.
func TestScore_TighterFitScoresHigherOnTie(t *testing.T) {
	const (
		req       = "40000000000" // 40 GB
		fragRatio = 0.10
	)
	srv := newMockDaemon(t, map[string]telemetry.NodeMetrics{
		"tight-fit": {NodeName: "tight-fit", VRAMAvailableBytes: 44_000_000_000, FragmentationRatio: fragRatio},
		"loose-fit": {NodeName: "loose-fit", VRAMAvailableBytes: 80_000_000_000, FragmentationRatio: fragRatio},
	})
	defer srv.Close()
	p := newPlugin(t, srv.URL)

	state := framework.NewCycleState()
	runPreFilter(t, p, state, req)

	scoreTight, _ := p.(framework.ScorePlugin).Score(context.Background(), state, newPod(req), "tight-fit")
	scoreLoose, _ := p.(framework.ScorePlugin).Score(context.Background(), state, newPod(req), "loose-fit")

	if scoreTight <= scoreLoose {
		t.Errorf("tight-fit node (score %d) should outscore loose-fit node (score %d) as tie-break", scoreTight, scoreLoose)
	}
}

// ── Full pipeline test ────────────────────────────────────────────────────────

// TestFullPipeline_H100SelectedOverA10G is the end-to-end scenario from the
// design: a 40 GB LLM workload is submitted against a simulated H100 (80 GB,
// 8% frag) and A10G (24 GB, 25% frag). The A10G must be filtered out and the
// H100 must win the scoring round.
func TestFullPipeline_H100SelectedOverA10G(t *testing.T) {
	const req = "40000000000" // 40 GB
	srv := newMockDaemon(t, map[string]telemetry.NodeMetrics{
		"sim-node-h100": {
			NodeName:           "sim-node-h100",
			VRAMAvailableBytes: 72_000_000_000,
			FragmentationRatio: 0.08,
		},
		"sim-node-a10g": {
			NodeName:           "sim-node-a10g",
			VRAMAvailableBytes: 20_000_000_000, // 20 GB — fails Filter
			FragmentationRatio: 0.25,
		},
	})
	defer srv.Close()
	p := newPlugin(t, srv.URL)

	pod := newPod(req)
	state := framework.NewCycleState()
	runPreFilter(t, p, state, req)

	// A10G must be filtered out.
	st := p.(framework.FilterPlugin).Filter(
		context.Background(), state, pod, newNodeInfo("sim-node-a10g"),
	)
	if st.Code() != framework.Unschedulable {
		t.Errorf("A10G should be Unschedulable for 40 GB request, got %v", st)
	}

	// H100 must pass Filter.
	st = p.(framework.FilterPlugin).Filter(
		context.Background(), state, pod, newNodeInfo("sim-node-h100"),
	)
	if !st.IsSuccess() {
		t.Errorf("H100 should pass Filter for 40 GB request, got %v", st)
	}

	// H100 score must be in valid range.
	score, st := p.(framework.ScorePlugin).Score(
		context.Background(), state, pod, "sim-node-h100",
	)
	if !st.IsSuccess() {
		t.Fatalf("scoring H100: %v", st)
	}
	if score < 0 || score > 100 {
		t.Errorf("H100 score %d out of expected range [0, 100]", score)
	}
}

// ── Helper ────────────────────────────────────────────────────────────────────

// runPreFilter runs the PreFilter phase and fails the test immediately if it
// does not return a Success (or Skip-equivalent) status. This keeps test bodies
// focused on the phase under test.
func runPreFilter(t *testing.T, p framework.Plugin, state *framework.CycleState, vramAnnotation string) {
	t.Helper()
	_, status := p.(framework.PreFilterPlugin).PreFilter(
		context.Background(), state, newPod(vramAnnotation),
	)
	if status != nil && status.Code() != framework.Success {
		t.Fatalf("PreFilter failed unexpectedly: %v", status)
	}
}
