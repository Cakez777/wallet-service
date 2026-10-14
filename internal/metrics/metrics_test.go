package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/Cakez777/wallet-service/internal/wagering"
)

func TestExpositionContainsCountersGaugesAndHistogram(t *testing.T) {
	r := New()
	r.Result("http", wagering.KindBet, wagering.StatusProcessed, false, 30*time.Millisecond)
	r.Result("sqs", wagering.KindBet, wagering.StatusProcessed, true, 2*time.Millisecond)
	r.ConcurrencyConflict()
	r.SQSResult("dead_lettered")
	r.Gauge("wallet_outbox_lag_seconds", func() float64 { return 1.5 })

	var b strings.Builder
	r.Write(&b)
	out := b.String()
	for _, want := range []string{
		`wallet_wager_results_total{source="http",kind="BET",status="PROCESSED"} 1`,
		`wallet_wager_duplicates_total{source="sqs"} 1`,
		`wallet_concurrency_conflicts_total 1`,
		`wallet_sqs_messages_total{result="dead_lettered"} 1`,
		`wallet_sqs_dlq_total 1`,
		`wallet_outbox_lag_seconds 1.5`,
		`wallet_wager_duration_seconds_bucket{source="http",le="0.05"} 1`,
		`wallet_wager_duration_seconds_count{source="sqs"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("faltou %q na exposição:\n%s", want, out)
		}
	}
}

func TestObserverAdaptersIncrementCounters(t *testing.T) {
	r := New()
	r.PendingRetry()
	r.ReconciliationDivergence()
	r.OutboxPublished()
	r.OutboxPublished()
	r.OutboxPublishFailed()
	for name, want := range map[string]uint64{
		"wallet_pending_retries_total":            1,
		"wallet_reconciliation_divergences_total": 1,
		"wallet_outbox_published_total":           2,
		"wallet_outbox_publish_failures_total":    1,
	} {
		if got := r.Counter(name); got != want {
			t.Errorf("%s = %d, esperado %d", name, got, want)
		}
	}
}
