package scheduler_test

// Chaos tests verify that the scheduler plugin handles telemetry daemon failures
// gracefully. The control plane must never crash due to downstream service issues —
// it should return a framework.Error status and let the scheduler retry or fall back.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"github.com/ray/gpu-vram-optimizer-k8s/internal/scheduler"
)

// filterWithChaos is a helper that runs the full PreFilter → Filter pipeline
// against a single node and returns the Filter status.
func filterWithChaos(t *testing.T, daemonURL, nodeName string) *framework.Status {
	t.Helper()
	p, err := scheduler.New(context.Background(), &scheduler.PluginConfig{
		TelemetryDaemonURL: daemonURL,
	}, nil)
	if err != nil {
		t.Fatalf("scheduler.New: %v", err)
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{scheduler.VRAMAnnotation: "8000000000"},
		},
	}
	state := framework.NewCycleState()

	if _, st := p.(framework.PreFilterPlugin).PreFilter(context.Background(), state, pod); st != nil && !st.IsSuccess() {
		t.Fatalf("PreFilter failed: %v", st)
	}

	ni := framework.NewNodeInfo()
	ni.SetNode(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}})
	return p.(framework.FilterPlugin).Filter(context.Background(), state, pod, ni)
}

// TestChaos_DaemonUnreachable verifies that Filter returns an Error (not a
// panic or hang) when the telemetry daemon cannot be reached.
func TestChaos_DaemonUnreachable(t *testing.T) {
	// Create a server and immediately close it so the port is unreachable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	st := filterWithChaos(t, closedURL, "any-node")
	if st.Code() != framework.Error {
		t.Errorf("expected Error when daemon is unreachable, got %v", st)
	}
}

// TestChaos_DaemonReturns500 verifies that a 500 from the telemetry daemon
// causes the plugin to return a framework.Error rather than silently succeed.
func TestChaos_DaemonReturns500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	st := filterWithChaos(t, srv.URL, "any-node")
	if st.Code() != framework.Error {
		t.Errorf("expected Error on daemon 500 response, got %v", st)
	}
}

// TestChaos_DaemonReturnsMalformedJSON verifies that malformed JSON from the
// daemon is handled gracefully with a framework.Error status.
func TestChaos_DaemonReturnsMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{this is not valid json`))
	}))
	defer srv.Close()

	st := filterWithChaos(t, srv.URL, "any-node")
	if st.Code() != framework.Error {
		t.Errorf("expected Error on malformed JSON, got %v", st)
	}
}

// TestChaos_DaemonReturns404_NodeUnschedulable verifies the 404 fallback path:
// when a node is not registered with the daemon, the plugin treats it as having
// 0 available VRAM, making it Unschedulable rather than causing an error.
// This prevents unknown nodes from silently receiving workloads.
func TestChaos_DaemonReturns404_NodeUnschedulable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	st := filterWithChaos(t, srv.URL, "unknown-node")
	if st.Code() != framework.Unschedulable {
		t.Errorf("expected Unschedulable for node unknown to daemon, got %v", st)
	}
}

// TestChaos_DaemonTimeout verifies that a slow daemon does not block the
// scheduler indefinitely. The client has a 2 s timeout; this test uses a
// context deadline shorter than that to assert the plugin respects cancellation.
func TestChaos_DaemonTimeout(t *testing.T) {
	// Slow server — never responds within our deadline.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the client's request context is canceled.
		<-r.Context().Done()
	}))
	defer srv.Close()

	p, err := scheduler.New(context.Background(), &scheduler.PluginConfig{
		TelemetryDaemonURL: srv.URL,
	}, nil)
	if err != nil {
		t.Fatalf("scheduler.New: %v", err)
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{scheduler.VRAMAnnotation: "8000000000"},
		},
	}

	state := framework.NewCycleState()
	if _, st := p.(framework.PreFilterPlugin).PreFilter(context.Background(), state, pod); st != nil && !st.IsSuccess() {
		t.Fatalf("PreFilter: %v", st)
	}

	ni := framework.NewNodeInfo()
	ni.SetNode(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "slow-node"}})

	// Use a context that expires well before the test timeout but after any
	// reasonable processing delay.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	st := p.(framework.FilterPlugin).Filter(ctx, state, pod, ni)
	if st.Code() != framework.Error {
		t.Errorf("expected Error when context deadline exceeded, got %v", st)
	}
}
