package metrics

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"example.com/wagering/internal/application"
	"go.uber.org/fx"
)

// Registry keeps only bounded labels; identifiers and financial payloads never enter metrics.
type Registry struct {
	mu                                                                                                  sync.Mutex
	results                                                                                             map[string]uint64
	duplicates, consumerRetries, consumerAcknowledged, outboxRetries, concurrencyConflicts, divergences uint64
	latencyCount                                                                                        uint64
	latencySum                                                                                          float64
	latencyBuckets                                                                                      [7]uint64
	dlqDepth, outboxLag                                                                                 float64
}

var bounds = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.5, 1}

func New() *Registry                                       { return &Registry{results: map[string]uint64{}} }
func AsApplicationMetrics(r *Registry) application.Metrics { return r }
func (r *Registry) ProcessResult(status string, duplicate bool, latency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch status {
	case "PROCESSED", "REJECTED", "PENDING_REFERENCE":
		r.results[status]++
	}
	if duplicate {
		r.duplicates++
	}
	seconds := latency.Seconds()
	r.latencyCount++
	r.latencySum += seconds
	for i, bound := range bounds {
		if seconds <= bound {
			r.latencyBuckets[i]++
		}
	}
}
func (r *Registry) ConcurrencyConflict()      { r.mu.Lock(); r.concurrencyConflicts++; r.mu.Unlock() }
func (r *Registry) ReconciliationDivergence() { r.mu.Lock(); r.divergences++; r.mu.Unlock() }
func (r *Registry) ConsumerRetry()            { r.mu.Lock(); r.consumerRetries++; r.mu.Unlock() }
func (r *Registry) ConsumerAcknowledged()     { r.mu.Lock(); r.consumerAcknowledged++; r.mu.Unlock() }
func (r *Registry) OutboxRetry()              { r.mu.Lock(); r.outboxRetries++; r.mu.Unlock() }
func (r *Registry) SetDLQDepth(n float64)     { r.mu.Lock(); r.dlqDepth = n; r.mu.Unlock() }
func (r *Registry) SetOutboxLag(n float64)    { r.mu.Lock(); r.outboxLag = n; r.mu.Unlock() }
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	for _, status := range []string{"PROCESSED", "REJECTED", "PENDING_REFERENCE"} {
		fmt.Fprintf(w, "wager_results_total{status=%q} %d\n", status, r.results[status])
	}
	fmt.Fprintf(w, "wager_duplicates_total %d\nwager_consumer_retries_total %d\nwager_consumer_acknowledged_total %d\nwager_outbox_retries_total %d\nwager_concurrency_conflicts_total %d\nwager_reconciliation_divergences_total %d\nwager_dlq_depth %g\nwager_outbox_lag_seconds %g\n", r.duplicates, r.consumerRetries, r.consumerAcknowledged, r.outboxRetries, r.concurrencyConflicts, r.divergences, r.dlqDepth, r.outboxLag)
	for i, bound := range bounds {
		fmt.Fprintf(w, "wager_processing_duration_seconds_bucket{le=%q} %d\n", fmt.Sprintf("%g", bound), r.latencyBuckets[i])
	}
	fmt.Fprintf(w, "wager_processing_duration_seconds_bucket{le=\"+Inf\"} %d\nwager_processing_duration_seconds_sum %g\nwager_processing_duration_seconds_count %d\n", r.latencyCount, r.latencySum, r.latencyCount)
}

var Module = fx.Module("metrics", fx.Provide(New, AsApplicationMetrics))
