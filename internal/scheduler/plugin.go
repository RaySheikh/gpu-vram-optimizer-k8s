package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"github.com/ray/gpu-vram-optimizer-k8s/internal/telemetry"
)

const (
	// Name is the unique identifier registered with the scheduler framework.
	Name = "VRAMPlugin"

	// VRAMAnnotation is the pod annotation that declares the workload's VRAM requirement in bytes.
	// Example: nvidia.com/gpu-vram-req: "8000000000"  (8 GB)
	VRAMAnnotation = "nvidia.com/gpu-vram-req"

	// stateKey is the key used to store per-scheduling-cycle state.
	stateKey = framework.StateKey(Name)

	// scoreWeightFrag is the weight of the fragmentation component in the final score (0–100).
	scoreWeightFrag = 90.0
	// scoreWeightFit is the weight of the best-fit (bin-packing) component in the final score (0–100).
	scoreWeightFit = 10.0
)

// preFilterState stores data computed once in PreFilter and reused across Filter/Score.
type preFilterState struct {
	requestedBytes int64
}

func (s *preFilterState) Clone() framework.StateData {
	return &preFilterState{requestedBytes: s.requestedBytes}
}

// PluginConfig holds configuration injected via the KubeSchedulerProfile.
// It implements runtime.Object so it can be passed to the plugin constructor.
type PluginConfig struct {
	// TelemetryDaemonURL is the base URL of the Telemetry Emulation Daemon in Service mode.
	// Mutually exclusive with DaemonSetPort.
	// Example: "http://telemetry-daemon-svc.gpu-scheduler.svc.cluster.local:8080"
	TelemetryDaemonURL string `json:"telemetryDaemonURL"`

	// DaemonSetPort enables DaemonSet mode: the plugin queries each node's daemon
	// pod directly at http://<nodeInternalIP>:<DaemonSetPort>.
	// Set this when running the telemetry daemon as a DaemonSet (default: "8080").
	DaemonSetPort string `json:"daemonSetPort"`
}

// DeepCopyObject satisfies runtime.Object. PluginConfig contains only value types.
func (c *PluginConfig) DeepCopyObject() runtime.Object {
	copy := *c
	return &copy
}

// GetObjectKind satisfies runtime.Object.
func (c *PluginConfig) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }

// NodeMetricsFetcher is the interface the plugin uses to query per-node VRAM metrics.
// TelemetryClient is the production implementation; in tests any httptest-backed client
// or stub that satisfies this interface can be substituted.
//
// nodeIP is optional — TelemetryClient uses it in DaemonSet mode to route
// directly to the daemon pod on the node. Pass empty to use Service mode.
type NodeMetricsFetcher interface {
	GetNodeMetrics(ctx context.Context, nodeName string, nodeIP ...string) (*telemetry.NodeMetrics, error)
}

// VRAMPlugin implements the Filter, Score, and PreFilter scheduler framework extension points.
// It enforces VRAM capacity constraints and ranks nodes using a Best-Fit Decreasing algorithm
// weighted by GPU memory fragmentation.
type VRAMPlugin struct {
	handle framework.Handle
	client NodeMetricsFetcher
}

// Compile-time assertions that VRAMPlugin satisfies the required framework interfaces.
var _ framework.PreFilterPlugin = &VRAMPlugin{}
var _ framework.FilterPlugin = &VRAMPlugin{}
var _ framework.ScorePlugin = &VRAMPlugin{}

// Name returns the plugin name, satisfying framework.Plugin.
func (p *VRAMPlugin) Name() string { return Name }

// New is the constructor called by the scheduler framework.
// The framework passes plugin args as *runtime.Unknown (raw JSON from the
// KubeSchedulerConfiguration), so we decode it ourselves rather than
// type-asserting directly to *PluginConfig.
func New(_ context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
	cfg := &PluginConfig{}
	if obj != nil {
		// Direct injection from tests passes *PluginConfig — accept it as-is.
		if direct, ok := obj.(*PluginConfig); ok {
			cfg = direct
		} else {
			// Production path: framework wraps YAML args as *runtime.Unknown.
			unknown, ok := obj.(*runtime.Unknown)
			if !ok {
				return nil, fmt.Errorf("unexpected plugin config type %T, want *runtime.Unknown or *PluginConfig", obj)
			}
			if err := json.Unmarshal(unknown.Raw, cfg); err != nil {
				return nil, fmt.Errorf("decoding VRAMPlugin config: %w", err)
			}
		}
	}
	if cfg.TelemetryDaemonURL == "" && cfg.DaemonSetPort == "" {
		return nil, fmt.Errorf("either telemetryDaemonURL or daemonSetPort must be set in plugin config")
	}
	var client NodeMetricsFetcher
	if cfg.DaemonSetPort != "" {
		client = NewDaemonSetClient(cfg.DaemonSetPort)
	} else {
		client = NewTelemetryClient(cfg.TelemetryDaemonURL)
	}
	return &VRAMPlugin{
		handle: h,
		client: client,
	}, nil
}

// --- PreFilter ---

// PreFilterExtensions returns nil because we have no per-node pre-filter state.
func (p *VRAMPlugin) PreFilterExtensions() framework.PreFilterExtensions { return nil }

// PreFilter parses the pod's VRAM annotation once and stores it in the CycleState,
// avoiding repeated annotation parsing during Filter and Score.
func (p *VRAMPlugin) PreFilter(
	ctx context.Context,
	state *framework.CycleState,
	pod *v1.Pod,
) (*framework.PreFilterResult, *framework.Status) {
	reqStr, ok := pod.Annotations[VRAMAnnotation]
	if !ok {
		// Pod does not request GPU VRAM — skip this plugin entirely.
		return nil, framework.NewStatus(framework.Skip)
	}

	reqBytes, err := strconv.ParseInt(reqStr, 10, 64)
	if err != nil || reqBytes <= 0 {
		return nil, framework.NewStatus(framework.Error,
			fmt.Sprintf("invalid %s annotation %q: must be a positive integer (bytes)", VRAMAnnotation, reqStr))
	}

	state.Write(stateKey, &preFilterState{requestedBytes: reqBytes})
	return nil, nil
}

// --- Filter ---

// Filter marks a node as Unschedulable if its available VRAM is less than the
// pod's request. This implements the capacity-check phase of the design.
func (p *VRAMPlugin) Filter(
	ctx context.Context,
	state *framework.CycleState,
	pod *v1.Pod,
	nodeInfo *framework.NodeInfo,
) *framework.Status {
	s, err := getPreFilterState(state)
	if err != nil {
		return framework.AsStatus(err)
	}

	nodeName := nodeInfo.Node().Name
	nodeIP := internalIP(nodeInfo.Node())
	metrics, err := p.client.GetNodeMetrics(ctx, nodeName, nodeIP)
	if err != nil {
		return framework.NewStatus(framework.Error,
			fmt.Sprintf("fetching telemetry for node %q: %v", nodeName, err))
	}

	if !filterNode(metrics.VRAMAvailableBytes, s.requestedBytes) {
		return framework.NewStatus(framework.Unschedulable,
			fmt.Sprintf("node %q: available VRAM %d bytes < requested %d bytes",
				nodeName, metrics.VRAMAvailableBytes, s.requestedBytes))
	}
	return nil
}

// --- Score ---

// ScoreExtensions returns nil as we do not implement NormalizeScore.
func (p *VRAMPlugin) ScoreExtensions() framework.ScoreExtensions { return nil }

// Score ranks surviving nodes using a weighted combination of:
//   - Fragmentation score (90 pts): lower fragmentation → higher score.
//   - Best-fit score     (10 pts): tighter fit (less leftover VRAM) → higher score.
//
// Combined formula:
//
//	score = 90*(1 - frag_ratio) + 10*(1 - (available-requested)/available)
func (p *VRAMPlugin) Score(
	ctx context.Context,
	state *framework.CycleState,
	pod *v1.Pod,
	nodeName string,
) (int64, *framework.Status) {
	s, err := getPreFilterState(state)
	if err != nil {
		return 0, framework.AsStatus(err)
	}

	// Resolve the node's internal IP from the framework snapshot so DaemonSet
	// mode can route directly to the per-node daemon pod.
	nodeIP := ""
	if p.handle != nil {
		if ni, err := p.handle.SnapshotSharedLister().NodeInfos().Get(nodeName); err == nil {
			nodeIP = internalIP(ni.Node())
		}
	}

	metrics, err := p.client.GetNodeMetrics(ctx, nodeName, nodeIP)
	if err != nil {
		return 0, framework.NewStatus(framework.Error,
			fmt.Sprintf("fetching telemetry for node %q: %v", nodeName, err))
	}

	return scoreNode(metrics.VRAMAvailableBytes, s.requestedBytes, metrics.FragmentationRatio), nil
}

// --- Pure Business Logic (exported for unit tests) ---

// FilterNode returns true if the node can satisfy the VRAM request.
// This is the capacity-check described in the Filter Phase of the design.
func filterNode(availableBytes, requestedBytes int64) bool {
	return availableBytes >= requestedBytes
}

// ScoreNode computes the composite Best-Fit Decreasing score for a node.
// Score range: 0–100.
//
//   - Fragmentation component (90 pts): 90 * (1 - fragRatio)
//     Nodes with less fragmented memory rank higher.
//   - Best-fit component (10 pts): 10 * (1 - leftover/available)
//     Nodes where the workload leaves the least unused space rank higher.
//     This drives tight bin-packing when fragmentation scores tie.
func scoreNode(availableBytes, requestedBytes int64, fragRatio float64) int64 {
	fragScore := scoreWeightFrag * (1.0 - fragRatio)

	fitScore := 0.0
	if availableBytes > 0 {
		leftover := float64(availableBytes - requestedBytes)
		fitScore = scoreWeightFit * (1.0 - leftover/float64(availableBytes))
	}

	return int64(fragScore + fitScore)
}

// --- Helpers ---

func getPreFilterState(state *framework.CycleState) (*preFilterState, error) {
	raw, err := state.Read(stateKey)
	if err != nil {
		return nil, fmt.Errorf("reading pre-filter state: %w", err)
	}
	s, ok := raw.(*preFilterState)
	if !ok {
		return nil, fmt.Errorf("invalid pre-filter state type %T", raw)
	}
	return s, nil
}

// internalIP returns the first InternalIP address of a node, or empty string
// if none is found. Used in DaemonSet mode to route telemetry queries directly
// to the daemon pod running on that node.
func internalIP(node *v1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == v1.NodeInternalIP {
			return addr.Address
		}
	}
	return ""
}
