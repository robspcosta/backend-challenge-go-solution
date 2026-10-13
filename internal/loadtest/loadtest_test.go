package loadtest

import (
	"context"
	"testing"
	"time"
)

func TestNearestRankPercentilesAndValidation(t *testing.T) {
	values := make([]time.Duration, 100)
	for i := range values {
		values[i] = time.Duration(i+1) * time.Millisecond
	}
	if percentile(values, 50) != 50 || percentile(values, 95) != 95 || percentile(values, 99) != 99 {
		t.Fatal("incorrect nearest-rank percentiles")
	}
	if _, err := Run(context.Background(), Config{Requests: -1}); err == nil {
		t.Fatal("invalid configuration accepted")
	}
}
