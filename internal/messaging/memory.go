package messaging

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Memory é uma fila FIFO em memória que reproduz o comportamento do SQS FIFO relevante para
// o serviço: ordem por grupo, um grupo nunca é entregue em paralelo (enquanto houver mensagem
// em voo), timeout de visibilidade, contagem de entregas, deduplicação por DeduplicationID e
// redirecionamento para a DLQ após maxReceive entregas. Usada nos testes.
type Memory struct {
	mu          sync.Mutex
	msgs        []*memMsg
	dedup       map[string]struct{}
	visibility  time.Duration
	maxReceive  int
	dlq         *Memory
	seq         int
	failSend    error // injeção de falha em testes
	failReceive error
}

type memMsg struct {
	id, body, group string
	visibleAt       time.Time
	receipt         string
	receives        int
}

// NewMemory cria a fila. visibility é o tempo em que uma mensagem recebida fica invisível;
// maxReceive > 0 com dlq != nil move a mensagem para a DLQ após essa quantidade de entregas.
func NewMemory(visibility time.Duration, maxReceive int, dlq *Memory) *Memory {
	if visibility <= 0 {
		visibility = 30 * time.Second
	}
	return &Memory{dedup: map[string]struct{}{}, visibility: visibility, maxReceive: maxReceive, dlq: dlq}
}

// FailSend faz Send retornar err (nil desliga). Para testes de recuperação.
func (q *Memory) FailSend(err error) { q.mu.Lock(); q.failSend = err; q.mu.Unlock() }

// FailReceive faz Receive retornar err (nil desliga).
func (q *Memory) FailReceive(err error) { q.mu.Lock(); q.failReceive = err; q.mu.Unlock() }

func (q *Memory) Send(_ context.Context, in SendInput) error {
	if in.GroupID == "" || in.DeduplicationID == "" {
		return fmt.Errorf("fifo queue requires GroupID and DeduplicationID")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failSend != nil {
		return q.failSend
	}
	if _, dup := q.dedup[in.DeduplicationID]; dup {
		return nil // a fila descarta silenciosamente, como o SQS
	}
	q.dedup[in.DeduplicationID] = struct{}{}
	q.seq++
	q.msgs = append(q.msgs, &memMsg{id: fmt.Sprintf("m-%d", q.seq), body: in.Body, group: in.GroupID})
	return nil
}

func (q *Memory) Receive(ctx context.Context, max int, wait time.Duration) ([]Message, error) {
	deadline := time.Now().Add(wait)
	for {
		out, err := q.receiveNow(max)
		if err != nil || len(out) > 0 {
			return out, err
		}
		if time.Now().After(deadline) {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (q *Memory) receiveNow(max int) ([]Message, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failReceive != nil {
		return nil, q.failReceive
	}
	now := time.Now()
	blocked := map[string]bool{}
	var out []Message
	kept := q.msgs[:0:0]
	for _, m := range q.msgs {
		if blocked[m.group] {
			kept = append(kept, m)
			continue
		}
		if m.visibleAt.After(now) { // em voo ou em atraso: bloqueia o resto do grupo
			blocked[m.group] = true
			kept = append(kept, m)
			continue
		}
		if len(out) >= max {
			kept = append(kept, m)
			continue
		}
		if q.maxReceive > 0 && q.dlq != nil && m.receives >= q.maxReceive {
			q.dlq.mu.Lock()
			q.dlq.seq++
			q.dlq.msgs = append(q.dlq.msgs, &memMsg{id: fmt.Sprintf("m-%d", q.dlq.seq), body: m.body, group: m.group})
			q.dlq.mu.Unlock()
			continue // removida desta fila
		}
		m.receives++
		q.seq++
		m.receipt = fmt.Sprintf("r-%d", q.seq)
		m.visibleAt = now.Add(q.visibility)
		blocked[m.group] = true
		out = append(out, Message{ID: m.id, Body: m.body, ReceiptHandle: m.receipt, ReceiveCount: m.receives, GroupID: m.group})
		kept = append(kept, m)
	}
	q.msgs = kept
	return out, nil
}

func (q *Memory) Delete(_ context.Context, receipt string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, m := range q.msgs {
		if m.receipt == receipt && receipt != "" {
			q.msgs = append(q.msgs[:i], q.msgs[i+1:]...)
			return nil
		}
	}
	return nil
}

func (q *Memory) Release(_ context.Context, receipt string, delay time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, m := range q.msgs {
		if m.receipt == receipt && receipt != "" {
			m.visibleAt = time.Now().Add(delay)
			m.receipt = ""
			return nil
		}
	}
	return nil
}

func (q *Memory) Ping(context.Context) error { return nil }

// Depth é o total de mensagens na fila (visíveis, em voo ou em atraso).
func (q *Memory) Depth() int { q.mu.Lock(); defer q.mu.Unlock(); return len(q.msgs) }

// Bodies devolve os corpos das mensagens presentes, em ordem (para asserções).
func (q *Memory) Bodies() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]string, 0, len(q.msgs))
	for _, m := range q.msgs {
		out = append(out, m.body)
	}
	return out
}
