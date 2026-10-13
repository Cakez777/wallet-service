//go:build integration

package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cakez777/wallet-service/internal/messaging"
	"github.com/Cakez777/wallet-service/internal/testutil"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", testutil.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seed insere eventos: aggregates agregados com perAgg eventos cada, numerados em n (global, crescente).
func seed(t *testing.T, db *sql.DB, aggregates, perAgg int) (ids []string) {
	t.Helper()
	n := 0
	for k := 0; k < perAgg; k++ {
		for a := 0; a < aggregates; a++ {
			agg := fmt.Sprintf("00000000-0000-0000-0000-%012d", a)
			id := uuid.NewString()
			payload, _ := json.Marshal(map[string]any{"agg": agg, "k": k, "n": n})
			if _, err := db.Exec(`
				INSERT INTO outbox_events (event_id, aggregate_id, aggregate_type, event_type, payload, occurred_at, next_attempt_at)
				VALUES ($1,$2,'Test','TestEvent',$3, now(), now())`, id, agg, payload); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
			n++
		}
	}
	return ids
}

func unpublished(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// countingQueue conta cada Send (antes da deduplicação da fila) e guarda a ordem de chegada.
type countingQueue struct {
	*messaging.Memory
	sends atomic.Int64
}

func (q *countingQueue) Send(ctx context.Context, in messaging.SendInput) error {
	q.sends.Add(1)
	return q.Memory.Send(ctx, in)
}

type obs struct{ ok, fail atomic.Int64 }

func (o *obs) OutboxPublished()     { o.ok.Add(1) }
func (o *obs) OutboxPublishFailed() { o.fail.Add(1) }

func drain(t *testing.T, p *Publisher, db *sql.DB) {
	t.Helper()
	end := time.Now().Add(15 * time.Second)
	for unpublished(t, db) > 0 {
		if time.Now().After(end) {
			t.Fatalf("outbox não esvaziou: %d pendentes", unpublished(t, db))
		}
		if _, err := p.PublishBatch(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPublishesEverythingInOrderPerAggregate(t *testing.T) {
	db := newDB(t)
	seed(t, db, 5, 6)
	q := &countingQueue{Memory: messaging.NewMemory(time.Minute, 0, nil)}
	p := New(db, q, Config{BatchSize: 7}, nil)
	drain(t, p, db)

	if q.Depth() != 30 {
		t.Fatalf("mensagens na fila = %d, esperado 30", q.Depth())
	}
	last := map[string]int{}
	for _, body := range q.Bodies() {
		var e struct {
			Agg string `json:"agg"`
			K   int    `json:"k"`
		}
		_ = json.Unmarshal([]byte(body), &e)
		if prev, ok := last[e.Agg]; ok && e.K != prev+1 {
			t.Fatalf("ordem quebrada no agregado %s: %d depois de %d", e.Agg, e.K, prev)
		}
		last[e.Agg] = e.K
	}
}

func TestSeveralPublishersNeverPublishTheSameEventTwice(t *testing.T) {
	db := newDB(t)
	seed(t, db, 20, 10) // 200 eventos
	q := &countingQueue{Memory: messaging.NewMemory(time.Minute, 0, nil)}
	o := &obs{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		p := New(db, q, Config{BatchSize: 10}, o)
		wg.Add(1)
		go func() {
			defer wg.Done()
			end := time.Now().Add(20 * time.Second)
			for unpublished(t, db) > 0 && time.Now().Before(end) {
				if _, err := p.PublishBatch(context.Background()); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if n := unpublished(t, db); n != 0 {
		t.Fatalf("pendentes = %d", n)
	}
	if q.sends.Load() != 200 {
		t.Errorf("Send chamado %d vezes, esperado exatamente 200 (sem publicação dupla)", q.sends.Load())
	}
	if o.ok.Load() != 200 {
		t.Errorf("métrica de publicados = %d", o.ok.Load())
	}
	// ordem por agregado também com 4 publicadores
	last := map[string]int{}
	for _, body := range q.Bodies() {
		var e struct {
			Agg string `json:"agg"`
			K   int    `json:"k"`
		}
		_ = json.Unmarshal([]byte(body), &e)
		if prev, ok := last[e.Agg]; ok && e.K != prev+1 {
			t.Fatalf("ordem quebrada no agregado %s: %d depois de %d", e.Agg, e.K, prev)
		}
		last[e.Agg] = e.K
	}
}

func TestBrokerOutageBacksOffThenRecovers(t *testing.T) {
	db := newDB(t)
	seed(t, db, 1, 3)
	mem := messaging.NewMemory(time.Minute, 0, nil)
	mem.FailSend(errors.New("SQS indisponível"))
	o := &obs{}
	p := New(db, mem, Config{BaseDelay: 150 * time.Millisecond, MaxDelay: time.Second}, o)

	n, err := p.PublishBatch(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v; com a fila fora nada é publicado e não há erro fatal", n, err)
	}
	var attempts int
	var lastErr sql.NullString
	var wait float64
	if err := db.QueryRow(`SELECT attempts, last_error, EXTRACT(EPOCH FROM (next_attempt_at - now())) FROM outbox_events ORDER BY seq LIMIT 1`).
		Scan(&attempts, &lastErr, &wait); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || !lastErr.Valid || wait <= 0 {
		t.Errorf("attempts=%d last_error=%v espera=%.2fs: esperado 1 tentativa, erro gravado e backoff futuro", attempts, lastErr, wait)
	}
	if n, _ := p.PublishBatch(context.Background()); n != 0 {
		t.Errorf("dentro do backoff nada deve ser tentado, publicou %d", n)
	}
	if o.fail.Load() != 1 {
		t.Errorf("falhas contadas = %d, esperado 1", o.fail.Load())
	}

	mem.FailSend(nil) // a fila volta
	drain(t, p, db)
	if mem.Depth() != 3 {
		t.Errorf("após a recuperação a fila tem %d, esperado 3", mem.Depth())
	}
	var stale int
	_ = db.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE last_error IS NOT NULL`).Scan(&stale)
	if stale != 0 {
		t.Errorf("last_error deveria ser limpo após publicar, restam %d", stale)
	}
}

func TestCrashedPublisherLeaseExpiresAndEventIsRepublishedWithSameID(t *testing.T) {
	db := newDB(t)
	ids := seed(t, db, 1, 1)
	mem := messaging.NewMemory(time.Minute, 0, nil)

	// publicador A reivindica e "morre": a concessão fica, nada é publicado nem marcado
	a := New(db, mem, Config{Lease: 300 * time.Millisecond}, nil)
	got, err := a.claim(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("claim: %v %v", got, err)
	}

	b := New(db, mem, Config{}, nil)
	if n, _ := b.PublishBatch(context.Background()); n != 0 {
		t.Fatalf("com a concessão válida, outro publicador não pode pegar o evento (publicou %d)", n)
	}
	time.Sleep(400 * time.Millisecond)
	if n, _ := b.PublishBatch(context.Background()); n != 1 {
		t.Fatalf("concessão expirada: outro publicador deveria assumir (publicou %d)", n)
	}
	if mem.Depth() != 1 || unpublished(t, db) != 0 {
		t.Errorf("fila=%d pendentes=%d", mem.Depth(), unpublished(t, db))
	}

	// caso pior: A estava vivo, publicou, mas não conseguiu marcar -> republicação com o mesmo id
	ids2 := seed(t, db, 1, 1)
	_ = mem.Send(context.Background(), messaging.SendInput{Body: "{}", GroupID: "g", DeduplicationID: ids2[0]})
	before := mem.Depth()
	drain(t, b, db)
	if mem.Depth() != before {
		t.Errorf("republicar com o mesmo eventId deve ser descartado pela fila (antes=%d depois=%d)", before, mem.Depth())
	}
	_ = ids
}

func TestStatsReportBacklog(t *testing.T) {
	db := newDB(t)
	seed(t, db, 2, 2)
	p := New(db, messaging.NewMemory(time.Minute, 0, nil), Config{}, nil)
	st, err := p.Stats(context.Background())
	if err != nil || st.Unpublished != 4 {
		t.Fatalf("stats: %+v %v", st, err)
	}
	drain(t, p, db)
	st, _ = p.Stats(context.Background())
	if st.Unpublished != 0 || st.OldestAge != 0 {
		t.Errorf("sem pendências: %+v", st)
	}
}
