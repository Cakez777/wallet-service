package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/Cakez777/wallet-service/internal/wallet"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	// Configura logger estruturado JSON
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable"
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		slog.Error("erro ao abrir conexão com o banco de dados", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		slog.Error("erro ao conectar no banco de dados", "error", err)
		os.Exit(1)
	}

	walletRepo := wallet.NewRepository(db)
	walletService := wallet.NewService(walletRepo)
	walletHandler := wallet.NewHandler(walletService)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json")
		if err := db.PingContext(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"status": "unavailable"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/wallets", walletHandler.HandleCreateWallet)
	mux.HandleFunc("/wallets/", func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) > len("/wallets/") {
			// Direciona para depósito ou saque com base no sufixo da URL
			if len(r.URL.Path) >= 8 && r.URL.Path[len(r.URL.Path)-8:] == "/deposit" {
				walletHandler.HandleDeposit(w, r)
				return
			}
			if len(r.URL.Path) >= 9 && r.URL.Path[len(r.URL.Path)-9:] == "/withdraw" {
				walletHandler.HandleWithdraw(w, r)
				return
			}
		}
		http.NotFound(w, r)
	})

	slog.Info("servidor iniciado com sucesso", "port", "8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		slog.Error("servidor encerrado com erro", "error", err)
		os.Exit(1)
	}
}
