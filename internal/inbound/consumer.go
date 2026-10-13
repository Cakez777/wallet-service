// Package inbound consome comandos de aposta da fila SQS e os processa pelo MESMO caso de uso
// (app.Service) usado pela API HTTP.
//
// Garantias:
//   - Entrega pelo menos uma vez da fila + inbox transacional = efeito exatamente uma vez:
//     a linha da inbox (consumidor, messageId) é gravada na MESMA transação SQL que altera o
//     saldo. Uma segunda entrega encontra a linha, é reconhecida e não reprocessa.
//   - A mensagem só é apagada da fila DEPOIS do commit. Se o processo cair entre o commit e o
//     delete, a mensagem volta e é descartada como duplicata.
//   - Falhas transitórias (banco fora, deadlock) devolvem a mensagem à fila com backoff
//     exponencial; após maxReceiveCount entregas a própria fila a move para a DLQ (redrive).
//   - Mensagens "veneno" (JSON inválido, campos inválidos, conflito de idempotência) vão
//     direto para a DLQ, sem consumir retentativas.
//   - Ordem por carteira: MessageGroupId = walletId. A fila não entrega a próxima mensagem do
//     grupo enquanto a anterior estiver em voo, então operações da mesma carteira são
//     processadas em ordem e nunca em paralelo, mesmo com várias instâncias.
//   - Encerramento gracioso: ao cancelar o contexto o consumidor para de buscar mensagens,
//     conclui a que está processando e retorna.
package inbound

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Cakez777/wallet-service/internal/app"
	"github.com/Cakez777/wallet-service/internal/messaging"
	"github.com/Cakez777/wallet-service/internal/money"
	"github.com/Cakez777/wallet-service/internal/wagering"
)

// ErrInboxConflict: o mesmo messageId chegou com conteúdo diferente.
var ErrInboxConflict = errors.New("message id already used with a different payload")

// CommandMessage é o corpo (JSON) das mensagens da fila de comandos. Equivale ao corpo HTTP
// de POST /wagering/transactions, com a chave de idempotência no corpo em vez do cabeçalho.
type CommandMessage struct {
	ProviderID                     string             `json:"providerId"`
	IdempotencyKey                 string             `json:"idempotencyKey"`
	ExternalTransactionID          string             `json:"externalTransactionId"`
	PlayerID                       string             `json:"playerId"`
	WalletID                       string             `json:"walletId"`
	RoundID                        string             `json:"roundId"`
	GameID                         string             `json:"gameId"`
	Kind                           string             `json:"kind"`
	Money                          wagering.MoneyData `json:"money"`
	ReferenceExternalTransactionID string             `json:"referenceExternalTransactionId,omitempty"`
	CorrelationID                  string             `json:"correlationId,omitempty"`
}

// Decode converte e valida o corpo da mensagem.
func Decode(body string) (wagering.Command, error) {
	var m CommandMessage
	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return wagering.Command{}, fmt.Errorf("malformed message: %w", err)
	}
	mo, err := money.Parse(m.Money.Amount, m.Money.Currency)
	if err != nil {
		return wagering.Command{}, &wagering.ValidationError{Field: "money", Reason: err.Error()}
	}
	cmd := wagering.Command{
		ProviderID: m.ProviderID, ExternalTransactionID: m.ExternalTransactionID, IdempotencyKey: m.IdempotencyKey,
		PlayerID: m.PlayerID, WalletID: m.WalletID, RoundID: m.RoundID, GameID: m.GameID,
		Kind: wagering.Kind(m.Kind), Money: mo, ReferenceExternalTransactionID: m.ReferenceExternalTransactionID,
		CorrelationID: m.CorrelationID,
	}
	if err := cmd.Validate(); err != nil {
		return wagering.Command{}, err
	}
	return cmd, nil
}

// Encode gera o corpo da mensagem (usado por produtores internos e testes).
func Encode(m CommandMessage) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// Processor é o caso de uso (app.Service).
type Processor interface {
	ProcessWith(ctx context.Context, cmd wagering.Command, h app.Hooks) (app.Result, error)
}

// Observer recebe o resultado de cada mensagem: acked, duplicate, retried, dead_lettered.
type Observer interface{ SQSResult(result string) }

type noopObs struct{}

func (noopObs) SQSResult(string) {}

// Config do consumidor.
type Config struct {
	Name      string        // nome na inbox (padrão "wager-commands")
	Workers   int           // goroutines de consumo (padrão 4)
	Batch     int           // mensagens por Receive (1..10, padrão 10)
	Wait      time.Duration // long polling (padrão 2s)
	BaseDelay time.Duration // backoff da 1ª retentativa (padrão 1s)
	MaxDelay  time.Duration // teto do backoff (padrão 60s)
	HandleTTL time.Duration // limite para processar uma mensagem (padrão 30s)
}

func (c Config) withDefaults() Config {
	if c.Name == "" {
		c.Name = "wager-commands"
	}
	if c.Workers <= 0 {
		c.Workers = 4
	}
	if c.Batch <= 0 || c.Batch > 10 {
		c.Batch = 10
	}
	if c.Wait <= 0 {
		c.Wait = 2 * time.Second
	}
	if c.BaseDelay <= 0 {
		c.BaseDelay = time.Second
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = time.Minute
	}
	if c.HandleTTL <= 0 {
		c.HandleTTL = 30 * time.Second
	}
	return c
}

// Consumer lê a fila de comandos.
type Consumer struct {
	queue messaging.Queue
	dlq   messaging.Queue // opcional: destino direto das mensagens veneno
	svc   Processor
	cfg   Config
	obs   Observer
}

// New cria o consumidor. dlq pode ser nil (então mensagens veneno apenas retornam à fila e
// seguem para a DLQ pelo redrive da própria fila).
func New(q, dlq messaging.Queue, svc Processor, cfg Config, obs Observer) *Consumer {
	if obs == nil {
		obs = noopObs{}
	}
	return &Consumer{queue: q, dlq: dlq, svc: svc, cfg: cfg.withDefaults(), obs: obs}
}

// Run consome até ctx ser cancelado e retorna depois que todos os workers terminarem.
func (c *Consumer) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < c.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.worker(ctx)
		}()
	}
	wg.Wait()
}

func (c *Consumer) worker(ctx context.Context) {
	errDelay := time.Second
	for ctx.Err() == nil {
		msgs, err := c.queue.Receive(ctx, c.cfg.Batch, c.cfg.Wait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("falha ao receber da fila", "error", err, "retryIn", errDelay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(errDelay):
			}
			if errDelay < 30*time.Second {
				errDelay *= 2
			}
			continue
		}
		errDelay = time.Second
		failedGroups := map[string]bool{}
		for _, m := range msgs {
			// Preserva a ordem do grupo: se uma mensagem falhou, as seguintes do mesmo grupo
			// no lote voltam à fila sem serem processadas.
			if failedGroups[m.GroupID] {
				_ = c.queue.Release(context.WithoutCancel(ctx), m.ReceiptHandle, 0)
				continue
			}
			// A mensagem em andamento termina mesmo se o encerramento começar.
			hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.HandleTTL)
			ok := c.handle(hctx, m)
			cancel()
			if !ok {
				failedGroups[m.GroupID] = true
			}
		}
	}
}

func (c *Consumer) backoff(receiveCount int) time.Duration {
	d := c.cfg.BaseDelay
	for i := 1; i < receiveCount && d < c.cfg.MaxDelay; i++ {
		d *= 2
	}
	if d > c.cfg.MaxDelay {
		d = c.cfg.MaxDelay
	}
	return d
}

func isPermanent(err error) bool {
	return wagering.IsValidationError(err) || errors.Is(err, app.ErrIdempotencyConflict) ||
		errors.Is(err, app.ErrExternalTransactionConflict) || errors.Is(err, app.ErrWalletNotFound) ||
		errors.Is(err, ErrInboxConflict) || errors.Is(err, errMalformed)
}

var errMalformed = errors.New("malformed message")

// handle processa uma mensagem. Retorna false se ela NÃO foi concluída (devolvida à fila ou
// enviada à DLQ por falha), para que o chamador segure as mensagens seguintes do mesmo grupo.
func (c *Consumer) handle(ctx context.Context, m messaging.Message) bool {
	cmd, err := Decode(m.Body)
	if err != nil {
		return c.deadLetter(ctx, m, fmt.Errorf("%w: %v", errMalformed, err))
	}
	messageID := cmd.ProviderID + ":" + cmd.IdempotencyKey
	hash := cmd.PayloadHash()
	name := c.cfg.Name

	res, err := c.svc.ProcessWith(ctx, cmd, app.Hooks{
		Source: "sqs",
		Before: func(ctx context.Context, tx *sql.Tx) (bool, error) {
			r, err := tx.ExecContext(ctx, `
				INSERT INTO inbox_messages (consumer_name, message_id, payload_hash, received_at)
				VALUES ($1,$2,$3,now()) ON CONFLICT DO NOTHING`, name, messageID, hash)
			if err != nil {
				return false, err
			}
			if n, _ := r.RowsAffected(); n == 1 {
				return false, nil // primeira entrega
			}
			var prev string
			if err := tx.QueryRowContext(ctx,
				`SELECT payload_hash FROM inbox_messages WHERE consumer_name=$1 AND message_id=$2`,
				name, messageID).Scan(&prev); err != nil {
				return false, err
			}
			if prev != hash {
				return false, ErrInboxConflict
			}
			return true, nil // já processada: duplicata
		},
		After: func(ctx context.Context, tx *sql.Tx, _ app.Result) error {
			_, err := tx.ExecContext(ctx,
				`UPDATE inbox_messages SET completed_at=now() WHERE consumer_name=$1 AND message_id=$2`, name, messageID)
			return err
		},
	})
	if err != nil {
		if isPermanent(err) {
			return c.deadLetter(ctx, m, err)
		}
		delay := c.backoff(m.ReceiveCount)
		slog.Warn("falha transitória; mensagem volta à fila", "messageId", m.ID, "dedupKey", messageID,
			"receiveCount", m.ReceiveCount, "retryIn", delay, "error", err)
		if rerr := c.queue.Release(ctx, m.ReceiptHandle, delay); rerr != nil {
			slog.Error("falha ao devolver mensagem à fila", "messageId", m.ID, "error", rerr)
		}
		c.obs.SQSResult("retried")
		return false
	}

	// Commit feito: só agora a mensagem sai da fila.
	if derr := c.queue.Delete(ctx, m.ReceiptHandle); derr != nil {
		// Segue válido: a mensagem voltará e será reconhecida como duplicata pela inbox.
		slog.Error("falha ao apagar mensagem após o commit (será tratada como duplicata)", "messageId", m.ID, "error", derr)
	}
	if res.Transaction == nil || res.Replay {
		c.obs.SQSResult("duplicate")
	} else {
		c.obs.SQSResult("acked")
	}
	slog.Info("mensagem processada", "messageId", m.ID, "dedupKey", messageID, "replay", res.Replay,
		"correlationId", cmd.CorrelationID)
	return true
}

// deadLetter envia a mensagem veneno à DLQ e a apaga da fila principal.
func (c *Consumer) deadLetter(ctx context.Context, m messaging.Message, cause error) bool {
	slog.Error("mensagem inválida enviada à DLQ", "messageId", m.ID, "error", cause)
	if c.dlq != nil {
		err := c.dlq.Send(ctx, messaging.SendInput{Body: m.Body, GroupID: nonEmpty(m.GroupID, "poison"), DeduplicationID: "dlq-" + m.ID})
		if err != nil {
			// Não conseguiu mover: devolve; o redrive da fila a levará à DLQ depois.
			_ = c.queue.Release(ctx, m.ReceiptHandle, c.backoff(m.ReceiveCount))
			c.obs.SQSResult("retried")
			return false
		}
		_ = c.queue.Delete(ctx, m.ReceiptHandle)
	} else {
		_ = c.queue.Release(ctx, m.ReceiptHandle, 0)
	}
	c.obs.SQSResult("dead_lettered")
	return false
}

func nonEmpty(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
