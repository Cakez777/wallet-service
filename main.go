package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Cakez777/wallet-service/internal/wallet"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/fx"
)

// NewLogger configura o logger estruturado JSON
func NewLogger() *slog.Logger {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	return logger
}

// NewDatabase inicializa e gerencia a conexão com o PostgreSQL
func NewDatabase(lc fx.Lifecycle) (*sql.DB, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable"
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ctxPing, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if err := db.PingContext(ctxPing); err != nil {
				return err
			}
			slog.Info("conexão com o banco de dados estabelecida com sucesso")
			return nil
		},
		OnStop: func(ctx context.Context) error {
			slog.Info("fechando conexão com o banco de dados")
			return db.Close()
		},
	})

	return db, nil
}

// AuthMiddleware valida a presença de um Bearer Token corporativo/serviço
func AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mantém os Health Checks públicos para o Kubernetes/Docker
		if strings.HasPrefix(r.URL.Path, "/health/") {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Unauthorized: missing authorization header", http.StatusUnauthorized)
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			http.Error(w, "Unauthorized: invalid authorization format", http.StatusUnauthorized)
			return
		}

		token := parts[1]
		if token == "" {
			http.Error(w, "Unauthorized: invalid token", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Params para injetar dependências no servidor HTTP
type ServerParams struct {
	fx.In
	Lifecycle     fx.Lifecycle
	WalletHandler *wallet.Handler
	DB            *sql.DB
	Logger        *slog.Logger
}

// RegisterServer configura o mux, aplica o middleware e gerencia o ciclo de vida via Fx
func RegisterServer(p ServerParams) {
	mux := http.NewServeMux()

	// Health Checks (Públicos)
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json")
		if err := p.DB.PingContext(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"status": "unavailable"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Rotas da API (Protegidas)
	mux.HandleFunc("/wallets", p.WalletHandler.HandleCreateWallet)
	mux.HandleFunc("/wallets/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if len(path) > len("/wallets/") {
			switch {
			case strings.HasSuffix(path, "/deposit"):
				p.WalletHandler.HandleDeposit(w, r)
				return
			case strings.HasSuffix(path, "/withdraw"):
				p.WalletHandler.HandleWithdraw(w, r)
				return
			case strings.HasSuffix(path, "/bet"):
				p.WalletHandler.HandleBet(w, r)
				return
			case strings.HasSuffix(path, "/win"):
				p.WalletHandler.HandleWin(w, r)
				return
			case strings.HasSuffix(path, "/loss"):
				p.WalletHandler.HandleLoss(w, r)
				return
			case strings.HasSuffix(path, "/refund"):
				p.WalletHandler.HandleRefund(w, r)
				return
			case strings.HasSuffix(path, "/rollback"):
				p.WalletHandler.HandleRollback(w, r)
				return
			}
		}
		http.NotFound(w, r)
	})

	// Envolve o mux completo com o middleware de autenticação
	handlerWithAuth := AuthMiddleware(mux)

	server := &http.Server{
		Addr:    ":8080",
		Handler: handlerWithAuth,
	}

	p.Lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			go func() {
				p.Logger.Info("servidor HTTP iniciado com sucesso", "port", "8080")
				if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					p.Logger.Error("erro no servidor HTTP", "error", err)
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			p.Logger.Info("parando o servidor HTTP...")
			return server.Shutdown(ctx)
		},
	})
}

func main() {
	fx.New(
		fx.Provide(
			NewLogger,
			NewDatabase,
			wallet.NewRepository,
			wallet.NewService,
			wallet.NewHandler,
		),
		fx.Invoke(RegisterServer),
	).Run()
}
