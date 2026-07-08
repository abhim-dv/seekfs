package main

import "testing"

func TestLatencyStatsReportsP99(t *testing.T) {
	stats := latencyStats([]float64{100, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	want := map[string]float64{
		"min":    1,
		"median": 5,
		"p90":    9,
		"p95":    9,
		"p99":    9,
		"max":    100,
	}
	for key, expected := range want {
		if got := stats[key]; got != expected {
			t.Fatalf("%s = %v, want %v (stats=%v)", key, got, expected, stats)
		}
	}
}
