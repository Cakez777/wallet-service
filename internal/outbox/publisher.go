// Package outbox publica os eventos gravados na tabela outbox_events (padrão transactional
// outbox). Os eventos são gravados na mesma transação que altera o saldo; este publicador os
// entrega à fila de eventos DEPOIS do commit, com semântica "pelo menos uma vez".
//
// Vários publicadores (processos ou instâncias) podem rodar ao mesmo tempo:
//   - cada lote é reivindicado com FOR UPDATE SKIP LOCKED e uma concessão (lease) temporária
//     (locked_by/locked_until), então dois publicadores nunca pegam o mesmo evento;
//   - se um publicador morrer depois de reivindicar, a concessão expira e outro republica o
//     evento com o MESMO event_id; a fila FIFO descarta repetições (MessageDeduplicationId =
//     event_id) e os consumidores devem deduplicar por eventId;
//   - só o evento mais antigo não publicado de cada agregado é elegível, o que preserva a
//     ordem por agregado (MessageGroupId = aggregate_id) mesmo com vários publicadores.
package outbox

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/Cakez777/wallet-service/internal/messaging"
	"github.com/google/uuid"
)

// Observer recebe contagens operacionais (implementado por metrics.Registry).
type Observer interface {
	OutboxPublished()
	OutboxPublishFailed()
}

type noop struct{}

func (noop) OutboxPublished()     {}
func (noop) OutboxPublishFailed() {}

// Config do publicador.
type Config struct {
	BatchSize int           // eventos reivindicados por lote (padrão 20)
	Lease     time.Duration // validade da concessão (padrão 30s)
	BaseDelay time.Duration // backoff da 1ª falha (padrão 1s); dobra a cada falha
	MaxDelay  time.Duration // teto do backoff (padrão 1min)
}

func (c Config) withDefaults() Config {
	if c.BatchSize <= 0 {
		c.BatchSize = 20
	}
	if c.Lease <= 0 {
		c.Lease = 30 * time.Second
	}
	if c.BaseDelay <= 0 {
		c.BaseDelay = time.Second
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = time.Minute
	}
	return c
}

// Publisher lê a outbox e publica na fila.
type Publisher struct {
	db    *sql.DB
	queue messaging.Queue
	cfg   Config
	id    string
	obs   Observer
}

// New cria um publicador com identidade própria (usada nas concessões).
func New(db *sql.DB, q messaging.Queue, cfg Config, obs Observer) *Publisher {
	if obs == nil {
		obs = noop{}
	}
	return &Publisher{db: db, queue: q, cfg: cfg.withDefaults(), id: "publisher-" + uuid.NewString(), obs: obs}
}

type claimed struct {
	eventID, aggregateID, payload string
	attempts                      int
}

// claim reivindica até BatchSize eventos elegíveis e devolve-os.
func (p *Publisher) claim(ctx context.Context) ([]claimed, error) {
	rows, err := p.db.QueryContext(ctx, `
		WITH cand AS (
		  SELECT e.event_id FROM outbox_events e
		   WHERE e.published_at IS NULL
		     AND e.next_attempt_at <= now()
		     AND (e.locked_until IS NULL OR e.locked_until < now())
		     AND NOT EXISTS (SELECT 1 FROM outbox_events o
		                      WHERE o.aggregate_id = e.aggregate_id
		                        AND o.published_at IS NULL AND o.seq < e.seq)
		   ORDER BY e.seq
		   LIMIT $1
		   FOR UPDATE OF e SKIP LOCKED)
		UPDATE outbox_events o
		   SET locked_by = $2, locked_until = now() + make_interval(secs => $3)
		  FROM cand
		 WHERE o.event_id = cand.event_id
		RETURNING o.seq, o.event_id::text, o.aggregate_id::text, o.payload::text, o.attempts`,
		p.cfg.BatchSize, p.id, p.cfg.Lease.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type withSeq struct {
		seq int64
		claimed
	}
	var tmp []withSeq
	for rows.Next() {
		var c withSeq
		if err := rows.Scan(&c.seq, &c.eventID, &c.aggregateID, &c.payload, &c.attempts); err != nil {
			return nil, err
		}
		tmp = append(tmp, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// RETURNING não garante ordem: publicar por seq.
	for i := 1; i < len(tmp); i++ {
		for j := i; j > 0 && tmp[j].seq < tmp[j-1].seq; j-- {
			tmp[j], tmp[j-1] = tmp[j-1], tmp[j]
		}
	}
	out := make([]claimed, len(tmp))
	for i, c := range tmp {
		out[i] = c.claimed
	}
	return out, nil
}

func (p *Publisher) backoff(attempts int) time.Duration {
	d := p.cfg.BaseDelay
	max := p.cfg.MaxDelay

	if attempts <= 0 {
		if d > max {
			return max
		}
		return d
	}

	for i := 0; i < attempts && d < max; i++ {
		if d > max/2 {
			return max
		}
		d *= 2
	}

	if d > max {
		return max
	}
	return d
}

// PublishBatch reivindica e publica um lote. Devolve quantos eventos foram publicados.
func (p *Publisher) PublishBatch(ctx context.Context) (int, error) {
	batch, err := p.claim(ctx)
	if err != nil {
		return 0, err
	}
	published := 0
	for i, ev := range batch {
		if ctx.Err() != nil {
			p.release(batch[i:])
			return published, ctx.Err()
		}
		err := p.queue.Send(ctx, messaging.SendInput{Body: ev.payload, GroupID: ev.aggregateID, DeduplicationID: ev.eventID})
		if err != nil {
			p.obs.OutboxPublishFailed()
			slog.Warn("falha ao publicar evento da outbox", "eventId", ev.eventID, "attempts", ev.attempts+1, "error", err)
			if ferr := p.markFailed(ctx, ev, err); ferr != nil {
				slog.Error("falha ao registrar erro de publicação", "eventId", ev.eventID, "error", ferr)
			}
			continue
		}
		res, err := p.db.ExecContext(ctx, `
			UPDATE outbox_events
			   SET published_at = now(), locked_by = NULL, locked_until = NULL, last_error = NULL
			 WHERE event_id = $1 AND locked_by = $2 AND published_at IS NULL`, ev.eventID, p.id)
		if err != nil {
			// Publicado na fila mas não marcado: a concessão expira e o evento será republicado
			// com o mesmo eventId (a fila e os consumidores deduplicam).
			slog.Error("evento publicado mas não marcado na outbox", "eventId", ev.eventID, "error", err)
			continue
		}
		if n, _ := res.RowsAffected(); n == 1 {
			p.obs.OutboxPublished()
			published++
		}
	}
	return published, nil
}

func (p *Publisher) markFailed(ctx context.Context, ev claimed, cause error) error {
	msg := cause.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, err := p.db.ExecContext(ctx, `
		UPDATE outbox_events
		   SET attempts = attempts + 1,
		       next_attempt_at = now() + make_interval(secs => $3),
		       locked_by = NULL, locked_until = NULL, last_error = $4
		 WHERE event_id = $1 AND locked_by = $2`,
		ev.eventID, p.id, p.backoff(ev.attempts).Seconds(), msg)
	return err
}

// release devolve eventos reivindicados e não tentados (encerramento).
func (p *Publisher) release(evs []claimed) {
	if len(evs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, ev := range evs {
		_, _ = p.db.ExecContext(ctx,
			`UPDATE outbox_events SET locked_by=NULL, locked_until=NULL WHERE event_id=$1 AND locked_by=$2`, ev.eventID, p.id)
	}
}

// Run publica continuamente até ctx ser cancelado. Esvazia a outbox sem pausa e, quando não
// há nada elegível, espera interval.
func (p *Publisher) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		for {
			if ctx.Err() != nil {
				return
			}

			n, err := p.PublishBatch(ctx)
			if err != nil {
				if ctx.Err() == nil {
					slog.Error("publicador da outbox", "error", err)
				}
				break
			}

			if n == 0 {
				break
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Stats resume a outbox: eventos pendentes e idade do mais antigo.
type Stats struct {
	Unpublished int64
	OldestAge   time.Duration
}

// Stats consulta o estado atual da outbox (para métricas e diagnóstico).
func (p *Publisher) Stats(ctx context.Context) (Stats, error) {
	var n int64
	var secs float64
	err := p.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(EXTRACT(EPOCH FROM (now() - MIN(occurred_at))), 0)
		  FROM outbox_events WHERE published_at IS NULL`).Scan(&n, &secs)
	if err != nil {
		return Stats{}, fmt.Errorf("outbox stats: %w", err)
	}
	if secs < 0 {
		secs = 0
	}
	return Stats{Unpublished: n, OldestAge: time.Duration(secs * float64(time.Second))}, nil
}
