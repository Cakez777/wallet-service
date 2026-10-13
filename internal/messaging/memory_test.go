package messaging

import (
	"context"
	"testing"
	"time"
)

func send(t *testing.T, q *Memory, body, group, dedup string) {
	t.Helper()
	if err := q.Send(context.Background(), SendInput{Body: body, GroupID: group, DeduplicationID: dedup}); err != nil {
		t.Fatal(err)
	}
}

func recv(t *testing.T, q *Memory, max int) []Message {
	t.Helper()
	m, err := q.Receive(context.Background(), max, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMemoryGroupOrderingAndBlocking(t *testing.T) {
	q := NewMemory(time.Minute, 0, nil)
	send(t, q, "a1", "A", "1")
	send(t, q, "a2", "A", "2")
	send(t, q, "b1", "B", "3")

	got := recv(t, q, 10)
	if len(got) != 2 || got[0].Body != "a1" || got[1].Body != "b1" {
		t.Fatalf("esperava a1 e b1 (um por grupo, em ordem), veio %+v", got)
	}
	if more := recv(t, q, 10); len(more) != 0 {
		t.Fatalf("grupo A tem mensagem em voo: nada deveria ser entregue, veio %+v", more)
	}
	_ = q.Delete(context.Background(), got[0].ReceiptHandle)
	next := recv(t, q, 10)
	if len(next) != 1 || next[0].Body != "a2" {
		t.Fatalf("após apagar a1, a2 deveria ser entregue: %+v", next)
	}
}

func TestMemoryDeduplicationReleaseAndDLQ(t *testing.T) {
	dlq := NewMemory(time.Minute, 0, nil)
	q := NewMemory(30*time.Millisecond, 2, dlq)
	send(t, q, "x", "G", "same")
	send(t, q, "x-again", "G", "same") // descartada
	if q.Depth() != 1 {
		t.Fatalf("deduplicação falhou: %d", q.Depth())
	}

	m := recv(t, q, 1)
	if len(m) != 1 || m[0].ReceiveCount != 1 {
		t.Fatalf("1ª entrega: %+v", m)
	}
	_ = q.Release(context.Background(), m[0].ReceiptHandle, 0)
	m = recv(t, q, 1)
	if len(m) != 1 || m[0].ReceiveCount != 2 {
		t.Fatalf("2ª entrega: %+v", m)
	}
	time.Sleep(50 * time.Millisecond) // visibilidade expira sem Delete
	if m = recv(t, q, 1); len(m) != 0 {
		t.Fatalf("após maxReceive, a mensagem vai à DLQ: %+v", m)
	}
	if q.Depth() != 0 || dlq.Depth() != 1 {
		t.Fatalf("fila=%d dlq=%d", q.Depth(), dlq.Depth())
	}
}
