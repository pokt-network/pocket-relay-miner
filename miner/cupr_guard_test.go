//go:build test

package miner

import "testing"

func TestIsClaimCUPRConsistent(t *testing.T) {
	tests := []struct {
		name     string
		smstSum  uint64
		smstCnt  uint64
		cupr     uint64
		expected bool
	}{
		{
			name:     "uniform CUPR matches",
			smstSum:  1783 * 6312,
			smstCnt:  1783,
			cupr:     6312,
			expected: true,
		},
		{
			name:     "mixed weights (non-integer average) is inconsistent",
			smstSum:  11190188, // the observed incident value
			smstCnt:  1783,
			cupr:     6312,
			expected: false,
		},
		{
			name:     "uniform-old sum against changed (new) CUPR is inconsistent",
			smstSum:  1783 * 6276, // mined entirely at old CUPR
			smstCnt:  1783,
			cupr:     6312, // chain now uses new CUPR
			expected: false,
		},
		{
			name:     "unknown CUPR (zero) fails open (consistent)",
			smstSum:  1783 * 6276,
			smstCnt:  1783,
			cupr:     0,
			expected: true,
		},
		{
			name:     "single relay uniform",
			smstSum:  6312,
			smstCnt:  1,
			cupr:     6312,
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isClaimCUPRConsistent(tt.smstSum, tt.smstCnt, tt.cupr)
			if got != tt.expected {
				t.Fatalf("isClaimCUPRConsistent(%d, %d, %d) = %v, want %v",
					tt.smstSum, tt.smstCnt, tt.cupr, got, tt.expected)
			}
		})
	}
}
