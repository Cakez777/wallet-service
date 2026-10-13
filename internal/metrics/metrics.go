// Package metrics expõe métricas operacionais em formato de texto do Prometheus, sem
// dependências externas. Implementa app.Observer e os observadores do consumidor e da outbox.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Cakez777/wallet-service/internal/wagering"
)

var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

type histogram struct {
	counts []uint64 // acumulado por bucket
	sum    float64
	n      uint64
}

// Registry guarda todos os contadores. É seguro para uso concorrente.
type Registry struct {
	mu       sync.Mutex
	counters map[string]uint64     // chave: nome{labels}
	hists    map[string]*histogram // por source
	gauges   map[string]func() float64
	help     map[string]string
}

// New cria um registro vazio.
func New() *Registry {
	r := &Registry{
		counters: map[string]uint64{}, hists: map[string]*histogram{},
		gauges: map[string]func() float64{}, help: map[string]string{},
	}
	for n, h := range map[string]string{
		"wallet_wager_results_total":              "Operações de aposta concluídas, por origem, tipo e status.",
		"wallet_wager_duplicates_total":           "Entregas duplicadas respondidas sem reprocessar, por origem.",
		"wallet_concurrency_conflicts_total":      "Tentativas repetidas por deadlock, serialização ou unicidade concorrente.",
		"wallet_pending_retries_total":            "Retentativas de transações aguardando referência.",
		"wallet_reconciliation_divergences_total": "Divergências entre saldo e ledger detectadas.",
		"wallet_sqs_messages_total":               "Mensagens SQS processadas, por resultado (acked, retried, dead_lettered, duplicate).",
		"wallet_sqs_dlq_total":                    "Mensagens enviadas à DLQ pelo consumidor (veneno) ou por redrive.",
		"wallet_outbox_published_total":           "Eventos de outbox publicados com sucesso.",
		"wallet_outbox_publish_failures_total":    "Falhas ao publicar eventos de outbox.",
		"wallet_outbox_lag_seconds":               "Idade do evento não publicado mais antigo (0 se não há).",
		"wallet_outbox_unpublished":               "Eventos de outbox ainda não publicados.",
		"wallet_wager_duration_seconds":           "Latência do processamento de uma operação, por origem.",
	} {
		r.help[n] = h
	}
	return r
}

func key(name string, kv ...string) string {
	if len(kv) == 0 {
		return name
	}
	var b strings.Builder
	b.WriteString(name + "{")
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%s=%q`, kv[i], kv[i+1])
	}
	b.WriteByte('}')
	return b.String()
}

// Inc incrementa um contador rotulado.
func (r *Registry) Inc(name string, kv ...string) {
	r.mu.Lock()
	r.counters[key(name, kv...)]++
	r.mu.Unlock()
}

// Counter lê o valor atual de um contador (usado em testes).
func (r *Registry) Counter(name string, kv ...string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counters[key(name, kv...)]
}

// Gauge registra uma função avaliada a cada scrape.
func (r *Registry) Gauge(name string, f func() float64) {
	r.mu.Lock()
	r.gauges[name] = f
	r.mu.Unlock()
}

func (r *Registry) observe(source string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.hists[source]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(latencyBuckets))}
		r.hists[source] = h
	}
	s := d.Seconds()
	for i, b := range latencyBuckets {
		if s <= b {
			h.counts[i]++
		}
	}
	h.sum += s
	h.n++
}

// ----- app.Observer -----

func (r *Registry) Result(source string, kind wagering.Kind, status wagering.Status, replay bool, took time.Duration) {
	r.Inc("wallet_wager_results_total", "source", source, "kind", string(kind), "status", string(status))
	if replay {
		r.Inc("wallet_wager_duplicates_total", "source", source)
	}
	r.observe(source, took)
}
func (r *Registry) ConcurrencyConflict()      { r.Inc("wallet_concurrency_conflicts_total") }
func (r *Registry) PendingRetry()             { r.Inc("wallet_pending_retries_total") }
func (r *Registry) ReconciliationDivergence() { r.Inc("wallet_reconciliation_divergences_total") }

// Write grava a exposição no formato de texto do Prometheus.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := map[string]bool{}
	var keys []string
	for k := range r.counters {
		keys = append(keys, k)
		names[strings.SplitN(k, "{", 2)[0]] = true
	}
	sort.Strings(keys)
	var order []string
	for n := range names {
		order = append(order, n)
	}
	sort.Strings(order)
	for _, n := range order {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", n, r.help[n], n)
		for _, k := range keys {
			if k == n || strings.HasPrefix(k, n+"{") {
				fmt.Fprintf(w, "%s %d\n", k, r.counters[k])
			}
		}
	}
	var gn []string
	for n := range r.gauges {
		gn = append(gn, n)
	}
	sort.Strings(gn)
	for _, n := range gn {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", n, r.help[n], n, n, r.gauges[n]())
	}
	if len(r.hists) > 0 {
		n := "wallet_wager_duration_seconds"
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", n, r.help[n], n)
		var srcs []string
		for s := range r.hists {
			srcs = append(srcs, s)
		}
		sort.Strings(srcs)
		for _, s := range srcs {
			h := r.hists[s]
			for i, b := range latencyBuckets {
				fmt.Fprintf(w, "%s_bucket{source=%q,le=\"%g\"} %d\n", n, s, b, h.counts[i])
			}
			fmt.Fprintf(w, "%s_bucket{source=%q,le=\"+Inf\"} %d\n%s_sum{source=%q} %g\n%s_count{source=%q} %d\n",
				n, s, h.n, n, s, h.sum, n, s, h.n)
		}
	}
}

// Handler serve GET /metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Write(w)
	})
}

// ----- outbox.Observer -----

func (r *Registry) OutboxPublished()     { r.Inc("wallet_outbox_published_total") }
func (r *Registry) OutboxPublishFailed() { r.Inc("wallet_outbox_publish_failures_total") }

// ----- inbound.Observer -----

// SQSResult conta uma mensagem SQS por resultado: acked, duplicate, retried, dead_lettered.
func (r *Registry) SQSResult(result string) {
	r.Inc("wallet_sqs_messages_total", "result", result)
	if result == "dead_lettered" {
		r.Inc("wallet_sqs_dlq_total")
	}
}
