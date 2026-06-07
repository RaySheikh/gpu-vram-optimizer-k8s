package telemetry

// NodeMetrics represents the emulated VRAM state for a single simulated GPU node.
// This type is shared between the daemon (producer) and the scheduler client (consumer).
type NodeMetrics struct {
	NodeName           string  `json:"node_name"`
	VRAMTotalBytes     int64   `json:"vram_total_bytes"`
	VRAMAvailableBytes int64   `json:"vram_available_bytes"`
	FragmentationRatio float64 `json:"fragmentation_ratio"`
}
