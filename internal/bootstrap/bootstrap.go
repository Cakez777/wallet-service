// Package bootstrap compõe a aplicação com Uber Fx: configuração, banco, serviço,
// autenticação, API HTTP e worker de referências pendentes, com ciclo de vida controlado.
//
// Ordem de encerramento (inversa à de início): servidor HTTP deixa de aceitar entradas e
// conclui as requisições em andamento; o worker é cancelado e aguardado; só então o pool do
// banco é fechado.
package bootstrap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/Cakez777/wallet-service/internal/app"
	"github.com/Cakez777/wallet-service/internal/auth"
	"github.com/Cakez777/wallet-service/internal/httpapi"
	"github.com/Cakez777/wallet-service/internal/metrics"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/fx"
)

// Config é lida do ambiente e validada na inicialização.
type Config struct {
	HTTPAddr       string
	DatabaseURL    string
	OIDCIssuer     string // valor exato do claim iss
	OIDCJWKSURL    string // endpoint de chaves públicas do IdP
	Pending        app.Config
	Messaging      MessagingConfig
	WorkerInterval time.Duration
	ShutdownGrace  time.Duration
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}

	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, v)
	}

	return n, nil
}

// envDuration lê uma duração inteira expressa na unidade informada,
// rejeitando valores que excederiam o limite de time.Duration.
func envDuration(key string, def int, unit time.Duration) (time.Duration, error) {
	n, err := envInt(key, def)
	if err != nil {
		return 0, err
	}

	if unit <= 0 {
		return 0, fmt.Errorf("invalid duration unit for %s", key)
	}

	maxDuration := int64(^uint64(0) >> 1)
	if int64(n) > maxDuration/int64(unit) {
		return 0, fmt.Errorf("%s is too large", key)
	}

	return time.Duration(n) * unit, nil
}

// LoadConfig lê e valida a configuração. Não há valores padrão para o IdP: sem ele, a
// aplicação não sobe (autenticação é obrigatória).
func LoadConfig() (Config, error) {
	c := Config{
		HTTPAddr:    env("HTTP_ADDR", ":8080"),
		DatabaseURL: env("DATABASE_URL", "postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable"),
		OIDCIssuer:  os.Getenv("OIDC_ISSUER"),
		OIDCJWKSURL: os.Getenv("OIDC_JWKS_URL"),
	}

	if c.OIDCIssuer == "" || c.OIDCJWKSURL == "" {
		return Config{}, errors.New("OIDC_ISSUER and OIDC_JWKS_URL are required")
	}

	attempts, err := envInt("PENDING_MAX_ATTEMPTS", 8)
	if err != nil {
		return Config{}, err
	}

	baseDelay, err := envDuration("PENDING_BASE_DELAY_MS", 2000, time.Millisecond)
	if err != nil {
		return Config{}, err
	}

	maxDelay, err := envDuration("PENDING_MAX_DELAY_MS", 300000, time.Millisecond)
	if err != nil {
		return Config{}, err
	}

	if maxDelay < baseDelay {
		return Config{}, errors.New(
			"PENDING_MAX_DELAY_MS must be greater than or equal to PENDING_BASE_DELAY_MS",
		)
	}

	workerInterval, err := envDuration("WORKER_INTERVAL_MS", 1000, time.Millisecond)
	if err != nil {
		return Config{}, err
	}

	shutdownGrace, err := envDuration("SHUTDOWN_GRACE_SECONDS", 20, time.Second)
	if err != nil {
		return Config{}, err
	}

	c.Pending = app.Config{
		PendingMaxAttempts: attempts,
		PendingBaseDelay:   baseDelay,
		PendingMaxDelay:    maxDelay,
	}

	mc, err := loadMessagingConfig()
	if err != nil {
		return Config{}, err
	}

	c.Messaging = mc
	c.WorkerInterval = workerInterval
	c.ShutdownGrace = shutdownGrace

	return c, nil
}

func newDB(lc fx.Lifecycle, cfg Config) (*sql.DB, error) {
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(25)
	db.SetConnMaxLifetime(30 * time.Minute)

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			if err := db.PingContext(ctx); err != nil {
				return fmt.Errorf("postgres unreachable: %w", err)
			}

			slog.Info("conexão com o PostgreSQL estabelecida")
			return nil
		},
		OnStop: func(context.Context) error {
			slog.Info("fechando o pool do PostgreSQL")
			return db.Close()
		},
	})

	return db, nil
}

func newService(db *sql.DB, cfg Config, reg *metrics.Registry) *app.Service {
	return app.NewService(db, cfg.Pending).WithObserver(reg)
}

func newVerifier(cfg Config) httpapi.TokenVerifier {
	return auth.NewVerifier(cfg.OIDCIssuer, cfg.OIDCJWKSURL)
}

func newAPI(svc *app.Service, v httpapi.TokenVerifier, db *sql.DB, m *Messaging, reg *metrics.Registry) *httpapi.API {
	return httpapi.New(svc, v, func(ctx context.Context) error {
		if err := db.PingContext(ctx); err != nil {
			return err
		}
		return m.Ready(ctx)
	}).WithMetrics(reg.Handler())
}

func startServer(lc fx.Lifecycle, cfg Config, api *httpapi.API) {
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			ln, err := net.Listen("tcp", cfg.HTTPAddr)
			if err != nil {
				return err
			}

			slog.Info("servidor HTTP iniciado", "addr", ln.Addr().String())

			go func() {
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					slog.Error("servidor HTTP encerrou com erro", "error", err)
				}
			}()

			return nil
		},
		OnStop: func(ctx context.Context) error {
			slog.Info("encerrando servidor HTTP: sem novas conexões, concluindo requisições em andamento")

			ctx, cancel := context.WithTimeout(ctx, cfg.ShutdownGrace)
			defer cancel()

			return srv.Shutdown(ctx)
		},
	})
}

func startWorker(lc fx.Lifecycle, svc *app.Service, cfg Config) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() {
				defer close(done)
				svc.RunPendingWorker(ctx, cfg.WorkerInterval, 20)
			}()

			slog.Info("worker de referências pendentes iniciado")
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			cancel()

			select {
			case <-done:
				slog.Info("worker de referências pendentes encerrado")
				return nil
			case <-stopCtx.Done():
				return fmt.Errorf("worker did not stop in time: %w", stopCtx.Err())
			}
		},
	})
}

// Module agrupa toda a composição. A ordem dos Invoke define a ordem de encerramento (inversa
// à de início): o servidor HTTP para primeiro; depois consumidor SQS e publicador da outbox
// (concluem o que está em andamento); depois o worker de pendências; por fim o pool do banco.
func Module() fx.Option {
	return fx.Module("wallet",
		fx.Provide(LoadConfig, newDB, newService, newVerifier, newAPI, newMetrics, newMessaging),
		fx.Invoke(startWorker, startMessaging, startServer),
	)
}
