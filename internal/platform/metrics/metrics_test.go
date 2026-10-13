package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBoundedPrometheusMetrics(t *testing.T) {
	r := New()
	r.ProcessResult("PROCESSED", true, 20*time.Millisecond)
	r.ProcessResult("arbitrary-unbounded-label", false, time.Second)
	r.ConsumerRetry()
	r.ConsumerAcknowledged()
	r.OutboxRetry()
	r.ConcurrencyConflict()
	r.ReconciliationDivergence()
	r.SetDLQDepth(2)
	r.SetOutboxLag(3.5)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, fragment := range []string{"wager_results_total{status=\"PROCESSED\"} 1", "wager_duplicates_total 1", "wager_consumer_retries_total 1", "wager_consumer_acknowledged_total 1", "wager_outbox_retries_total 1", "wager_concurrency_conflicts_total 1", "wager_reconciliation_divergences_total 1", "wager_dlq_depth 2", "wager_outbox_lag_seconds 3.5", "wager_processing_duration_seconds_count 2"} {
		if !strings.Contains(body, fragment) {
			t.Errorf("missing %q in %s", fragment, body)
		}
	}
	if strings.Contains(body, "arbitrary-unbounded-label") {
		t.Fatal("unbounded status label exposed")
	}
}
