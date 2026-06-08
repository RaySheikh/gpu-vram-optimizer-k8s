package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ray/gpu-vram-optimizer-k8s/internal/telemetry"
)

// TelemetryClient fetches node VRAM metrics from the Telemetry Emulation Daemon.
//
// Two query modes are supported:
//
//  1. Service mode (default): queries a central URL (e.g. a ClusterIP Service).
//     The daemon at that URL must know about the requested node by name.
//
//  2. DaemonSet mode (daemonPort > 0): queries the daemon pod running directly
//     on the node via http://<nodeInternalIP>:<daemonPort>. Each pod only knows
//     its own node, so this avoids the 404 problem with per-node daemons.
type TelemetryClient struct {
	daemonURL  string
	daemonPort string // non-empty enables DaemonSet mode
	httpClient *http.Client
}

// NewTelemetryClient creates a client in Service mode pointed at daemonURL.
// Example daemonURL: "http://telemetry-daemon-svc.gpu-scheduler.svc.cluster.local:8080"
func NewTelemetryClient(daemonURL string) *TelemetryClient {
	return &TelemetryClient{
		daemonURL: daemonURL,
		httpClient: &http.Client{
			Timeout: 2 * time.Second,
		},
	}
}

// NewDaemonSetClient creates a client in DaemonSet mode. It will query
// http://<nodeInternalIP>:<port>/api/v1/nodes/<nodeName> so each daemon pod
// serves only the node it runs on.
func NewDaemonSetClient(port string) *TelemetryClient {
	return &TelemetryClient{
		daemonPort: port,
		httpClient: &http.Client{
			Timeout: 2 * time.Second,
		},
	}
}

// GetNodeMetrics fetches the VRAM metrics for a specific k8s node name.
// nodeIP is only used in DaemonSet mode; pass empty string for Service mode.
func (c *TelemetryClient) GetNodeMetrics(ctx context.Context, nodeName string, nodeIP ...string) (*telemetry.NodeMetrics, error) {
	var baseURL string
	if c.daemonPort != "" && len(nodeIP) > 0 && nodeIP[0] != "" {
		baseURL = fmt.Sprintf("http://%s:%s", nodeIP[0], c.daemonPort)
	} else {
		baseURL = c.daemonURL
	}
	url := fmt.Sprintf("%s/api/v1/nodes/%s", baseURL, nodeName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying telemetry daemon for node %q: %w", nodeName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// Node not registered with the daemon — treat as no available VRAM
		// so the Filter phase will mark it Unschedulable.
		return &telemetry.NodeMetrics{
			NodeName:           nodeName,
			VRAMAvailableBytes: 0,
			FragmentationRatio: 1.0,
		}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from telemetry daemon for node %q", resp.StatusCode, nodeName)
	}

	var m telemetry.NodeMetrics
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decoding metrics for node %q: %w", nodeName, err)
	}
	return &m, nil
}
