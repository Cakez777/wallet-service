package app

import (
	"time"

	"github.com/Cakez777/wallet-service/internal/wagering"
)

// Observer recebe eventos operacionais do serviço. A implementação real (métricas) vive
// em outro pacote; o serviço não depende dela.
type Observer interface {
	// Result é chamado uma vez por operação externa concluída (ou com erro).
	// status é o estado da transação, "ERROR" ou "DUPLICATE_SKIPPED".
	Result(source string, kind wagering.Kind, status wagering.Status, replay bool, took time.Duration)
	// ConcurrencyConflict conta tentativas repetidas por deadlock, falha de serialização ou
	// violação de unicidade causada por concorrência.
	ConcurrencyConflict()
	// PendingRetry conta retentativas de transações PENDING_REFERENCE.
	PendingRetry()
	// ReconciliationDivergence conta divergências detectadas entre saldo e ledger.
	ReconciliationDivergence()
}

type noopObserver struct{}

func (noopObserver) Result(string, wagering.Kind, wagering.Status, bool, time.Duration) {}
func (noopObserver) ConcurrencyConflict()                                               {}
func (noopObserver) PendingRetry()                                                      {}
func (noopObserver) ReconciliationDivergence()                                          {}
