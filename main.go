package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/Cakez777/wallet-service/internal/bootstrap"
	"go.uber.org/fx"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	fx.New(
		bootstrap.Module(),
		fx.StartTimeout(30*time.Second),
		fx.StopTimeout(30*time.Second),
	).Run()
}
