package autopilot_test

import (
	"testing"

	"github.com/wakiyamap/monautil"
	"github.com/wakiyamap/lnd/autopilot"
)

// TestMedian tests the Median method.
func TestMedian(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		values []monautil.Amount
		median monautil.Amount
	}{
		{
			values: []monautil.Amount{},
			median: 0,
		},
		{
			values: []monautil.Amount{10},
			median: 10,
		},
		{
			values: []monautil.Amount{10, 20},
			median: 15,
		},
		{
			values: []monautil.Amount{10, 20, 30},
			median: 20,
		},
		{
			values: []monautil.Amount{30, 10, 20},
			median: 20,
		},
		{
			values: []monautil.Amount{10, 10, 10, 10, 5000000},
			median: 10,
		},
	}

	for _, test := range testCases {
		res := autopilot.Median(test.values)
		if res != test.median {
			t.Fatalf("expected median %v, got %v", test.median, res)
		}
	}
}
