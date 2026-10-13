package bootstrap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/Cakez777/wallet-service/internal/app"
	"github.com/Cakez777/wallet-service/internal/inbound"
	"github.com/Cakez777/wallet-service/internal/messaging"
	"github.com/Cakez777/wallet-service/internal/metrics"
	"github.com/Cakez777/wallet-service/internal/outbox"
	"go.uber.org/fx"
)

// MessagingConfig configura SQS, o consumidor de comandos e o publicador da outbox.
type MessagingConfig struct {
	Enabled        bool
	Endpoint       string // vazio = AWS real; LocalStack: http://localstack:4566
	Region         string
	CommandsQueue  string
	CommandsDLQ    string
	EventsQueue    string
	EventsDLQ      string
	MaxReceive     int           // entregas antes de a fila mover a mensagem para a DLQ
	Visibility     time.Duration // deve ser maior que o tempo máximo de processamento (30s)
	Workers        int
	OutboxInterval time.Duration
	OutboxBatch    int
}

func loadMessagingConfig() (MessagingConfig, error) {
	m := MessagingConfig{
		Enabled:       env("MESSAGING_ENABLED", "true") != "false",
		Endpoint:      os.Getenv("SQS_ENDPOINT"),
		Region:        env("AWS_REGION", "us-east-1"),
		CommandsQueue: env("COMMANDS_QUEUE_NAME", "wager-transactions.fifo"),
		CommandsDLQ:   env("COMMANDS_DLQ_NAME", "wager-transactions-dlq.fifo"),
		EventsQueue:   env("EVENTS_QUEUE_NAME", "wallet-events.fifo"),
		EventsDLQ:     env("EVENTS_DLQ_NAME", "wallet-events-dlq.fifo"),
	}
	var err error
	if m.MaxReceive, err = envInt("SQS_MAX_RECEIVE", 5); err != nil {
		return m, err
	}
	vis, err := envInt("SQS_VISIBILITY_SECONDS", 60)
	if err != nil {
		return m, err
	}
	if vis < 31 {
		return m, errors.New("SQS_VISIBILITY_SECONDS must be greater than the 30s processing limit")
	}
	m.Visibility = time.Duration(vis) * time.Second
	if m.Workers, err = envInt("CONSUMER_WORKERS", 4); err != nil {
		return m, err
	}
	ms, err := envInt("OUTBOX_INTERVAL_MS", 500)
	if err != nil {
		return m, err
	}
	m.OutboxInterval = time.Duration(ms) * time.Millisecond
	if m.OutboxBatch, err = envInt("OUTBOX_BATCH", 20); err != nil {
		return m, err
	}
	return m, nil
}

func newMetrics() *metrics.Registry { return metrics.New() }

// Messaging guarda as filas provisionadas na inicialização, para a verificação de readiness.
type Messaging struct {
	enabled bool
	mu      sync.RWMutex
	cmds    messaging.Queue
	events  messaging.Queue
}

func newMessaging(cfg Config) *Messaging { return &Messaging{enabled: cfg.Messaging.Enabled} }

func (m *Messaging) set(cmds, events messaging.Queue) {
	m.mu.Lock()
	m.cmds, m.events = cmds, events
	m.mu.Unlock()
}

// Ready verifica as filas (readiness). Sem mensageria habilitada, sempre pronto.
func (m *Messaging) Ready(ctx context.Context) error {
	if m == nil || !m.enabled {
		return nil
	}
	m.mu.RLock()
	c, e := m.cmds, m.events
	m.mu.RUnlock()
	if c == nil || e == nil {
		return errors.New("queues not provisioned")
	}
	if err := c.Ping(ctx); err != nil {
		return fmt.Errorf("commands queue: %w", err)
	}
	if err := e.Ping(ctx); err != nil {
		return fmt.Errorf("events queue: %w", err)
	}
	return nil
}

// provision cria as filas, tentando de novo enquanto o SQS ainda está subindo.
func provision(ctx context.Context, c MessagingConfig) (cmds, cmdsDLQ, events *messaging.SQS, err error) {
	client, err := messaging.NewSQSClient(ctx, messaging.SQSConfig{Endpoint: c.Endpoint, Region: c.Region})
	if err != nil {
		return nil, nil, nil, err
	}
	for {
		cmds, cmdsDLQ, err = messaging.EnsureFIFOWithDLQ(ctx, client, c.CommandsQueue, c.CommandsDLQ, c.Visibility, c.MaxReceive)
		if err == nil {
			events, _, err = messaging.EnsureFIFOWithDLQ(ctx, client, c.EventsQueue, c.EventsDLQ, c.Visibility, c.MaxReceive)
		}
		if err == nil {
			return cmds, cmdsDLQ, events, nil
		}
		slog.Warn("SQS indisponível; nova tentativa", "error", err)
		select {
		case <-ctx.Done():
			return nil, nil, nil, fmt.Errorf("sqs unreachable: %w", err)
		case <-time.After(2 * time.Second):
		}
	}
}

func startMessaging(lc fx.Lifecycle, cfg Config, db *sql.DB, svc *app.Service, reg *metrics.Registry, m *Messaging) {
	if !cfg.Messaging.Enabled {
		slog.Info("mensageria desabilitada (MESSAGING_ENABLED=false)")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	lc.Append(fx.Hook{
		OnStart: func(startCtx context.Context) error {
			cmds, cmdsDLQ, events, err := provision(startCtx, cfg.Messaging)
			if err != nil {
				cancel()
				return err
			}
			m.set(cmds, events)

			consumer := inbound.New(cmds, cmdsDLQ, svc, inbound.Config{Workers: cfg.Messaging.Workers}, reg)
			publisher := outbox.New(db, events, outbox.Config{BatchSize: cfg.Messaging.OutboxBatch}, reg)
			gauge := func(f func(outbox.Stats) float64) func() float64 {
				return func() float64 {
					c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					st, err := publisher.Stats(c)
					if err != nil {
						return -1
					}
					return f(st)
				}
			}
			reg.Gauge("wallet_outbox_lag_seconds", gauge(func(s outbox.Stats) float64 { return s.OldestAge.Seconds() }))
			reg.Gauge("wallet_outbox_unpublished", gauge(func(s outbox.Stats) float64 { return float64(s.Unpublished) }))

			wg.Add(2)
			go func() { defer wg.Done(); consumer.Run(ctx) }()
			go func() { defer wg.Done(); publisher.Run(ctx, cfg.Messaging.OutboxInterval) }()
			slog.Info("consumidor SQS e publicador da outbox iniciados",
				"commandsQueue", cmds.URL(), "eventsQueue", events.URL(), "workers", cfg.Messaging.Workers)
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			slog.Info("encerrando consumidor SQS e publicador da outbox")
			cancel()
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
				return nil
			case <-stopCtx.Done():
				return fmt.Errorf("messaging did not stop in time: %w", stopCtx.Err())
			}
		},
	})
}
