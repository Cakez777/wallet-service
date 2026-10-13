// Package messaging define a abstração de fila usada pelo consumidor e pelo publicador da
// outbox. A implementação real (SQS) está em sqs.go; memory.go traz uma fila em memória com
// a mesma semântica (FIFO por grupo, visibilidade, contagem de entregas, DLQ) para testes.
package messaging

import (
	"context"
	"time"
)

// Message é uma mensagem recebida.
type Message struct {
	ID            string // identificador atribuído pela fila
	Body          string
	ReceiptHandle string
	ReceiveCount  int // quantas vezes foi entregue (inclui esta)
	GroupID       string
}

// SendInput descreve uma mensagem a publicar numa fila FIFO.
//
//	GroupID: mensagens do mesmo grupo são entregues em ordem e nunca em paralelo.
//	DeduplicationID: a fila descarta reenvios com o mesmo valor numa janela de 5 minutos.
type SendInput struct {
	Body            string
	GroupID         string
	DeduplicationID string
}

// Queue é a fila usada pelo serviço.
type Queue interface {
	Send(ctx context.Context, in SendInput) error
	// Receive espera até wait por até max mensagens. Retorna vazio quando não há nenhuma.
	Receive(ctx context.Context, max int, wait time.Duration) ([]Message, error)
	// Delete confirma o processamento: a mensagem não será mais entregue.
	Delete(ctx context.Context, receiptHandle string) error
	// Release torna a mensagem visível de novo após delay (backoff de retentativa).
	Release(ctx context.Context, receiptHandle string, delay time.Duration) error
	// Ping verifica a conectividade (readiness).
	Ping(ctx context.Context) error
}
