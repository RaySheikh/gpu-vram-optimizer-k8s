package scheduler

import (
	"testing"
)

// TestFilterNode validates the capacity-check phase of the design.
// A node must have available VRAM >= the requested amount to be schedulable.
func TestFilterNode(t *testing.T) {
	tests := []struct {
		name            string
		availableBytes  int64
		requestedBytes  int64
		wantSchedulable bool
	}{
		{
			name:            "exact fit — schedulable",
			availableBytes:  8_000_000_000,
			requestedBytes:  8_000_000_000,
			wantSchedulable: true,
		},
		{
			name:            "ample headroom — schedulable",
			availableBytes:  80_000_000_000, // 80 GB H100
			requestedBytes:  40_000_000_000, // 40 GB LLM request
			wantSchedulable: true,
		},
		{
			name:            "one byte short — unschedulable",
			availableBytes:  7_999_999_999,
			requestedBytes:  8_000_000_000,
			wantSchedulable: false,
		},
		{
			name:            "zero available — unschedulable",
			availableBytes:  0,
			requestedBytes:  8_000_000_000,
			wantSchedulable: false,
		},
		{
			name:            "small node, large request — unschedulable",
			availableBytes:  24_000_000_000, // 24 GB A10G
			requestedBytes:  40_000_000_000, // 40 GB LLM
			wantSchedulable: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := filterNode(tc.availableBytes, tc.requestedBytes)
			if got != tc.wantSchedulable {
				t.Errorf("filterNode(%d, %d) = %v, want %v",
					tc.availableBytes, tc.requestedBytes, got, tc.wantSchedulable)
			}
		})
	}
}

// TestScoreNode validates the Best-Fit Decreasing scoring formula.
// Higher score = better candidate for scheduling.
func TestScoreNode(t *testing.T) {
	tests := []struct {
		name           string
		availableBytes int64
		requestedBytes int64
		fragRatio      float64
		wantScore      int64
	}{
		{
			name:           "perfect node — zero fragmentation, exact fit",
			availableBytes: 8_000_000_000,
			requestedBytes: 8_000_000_000,
			fragRatio:      0.0,
			wantScore:      100, // 90*(1-0) + 10*(1-0/8GB) = 100
		},
		{
			name:           "fully fragmented node — lowest score",
			availableBytes: 80_000_000_000,
			requestedBytes: 8_000_000_000,
			fragRatio:      1.0,
			wantScore:      1, // 90*(1-1.0)=0, fit = 10*(1-72/80)=1
		},
		{
			name:           "low frag, tight fit — high score (H100 scenario)",
			availableBytes: 44_000_000_000, // 44 GB remaining
			requestedBytes: 40_000_000_000, // 40 GB request
			fragRatio:      0.08,
			wantScore:      91, // 90*0.92=82.8 + 10*(1-4/44)=9.09 ≈ 91
		},
		{
			name:           "moderate frag, loose fit — lower score (A10G scenario)",
			availableBytes: 20_000_000_000,
			requestedBytes: 8_000_000_000,
			fragRatio:      0.25,
			wantScore:      71, // 90*(1-0.25)=67.5 + 10*(1-12/20)=4.0 → 71 (truncated)
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := scoreNode(tc.availableBytes, tc.requestedBytes, tc.fragRatio)
			// Allow ±1 for floating-point truncation.
			diff := got - tc.wantScore
			if diff < -1 || diff > 1 {
				t.Errorf("scoreNode(%d, %d, %.2f) = %d, want ~%d",
					tc.availableBytes, tc.requestedBytes, tc.fragRatio, got, tc.wantScore)
			}
		})
	}
}

// TestScoreNodeBestFitTieBreaking verifies that when two nodes have identical
// fragmentation ratios, the node with the tighter fit (less leftover VRAM)
// receives a higher score — implementing the Best-Fit Decreasing tie-break.
func TestScoreNodeBestFitTieBreaking(t *testing.T) {
	const (
		requestedBytes = 40_000_000_000 // 40 GB LLM workload
		fragRatio      = 0.10           // identical fragmentation on both nodes
	)

	// Node A has exactly 44 GB available → 4 GB leftover after placement.
	scoreA := scoreNode(44_000_000_000, requestedBytes, fragRatio)
	// Node B has 80 GB available → 40 GB leftover after placement (much looser fit).
	scoreB := scoreNode(80_000_000_000, requestedBytes, fragRatio)

	if scoreA <= scoreB {
		t.Errorf("expected tighter-fit node A (score %d) > loose-fit node B (score %d), "+
			"but got the opposite — Best-Fit tie-breaking is broken", scoreA, scoreB)
	}
}

// TestScoreNodeFragmentationDominates verifies that the fragmentation component
// dominates the score, so a less-fragmented node always beats a more-fragmented
// node even when the fragmented node has a slightly tighter fit.
func TestScoreNodeFragmentationDominates(t *testing.T) {
	const requestedBytes = 40_000_000_000

	// Low-frag node: 8% frag, loose fit (80 GB available)
	scoreLowFrag := scoreNode(80_000_000_000, requestedBytes, 0.08)
	// High-frag node: 50% frag, tight fit (44 GB available)
	scoreHighFrag := scoreNode(44_000_000_000, requestedBytes, 0.50)

	if scoreLowFrag <= scoreHighFrag {
		t.Errorf("expected low-frag node (score %d) > high-frag node (score %d), "+
			"but fragmentation weighting is not working correctly", scoreLowFrag, scoreHighFrag)
	}
}
