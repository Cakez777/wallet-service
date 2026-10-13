// Package testutil reúne apoio aos testes de integração (PostgreSQL real).
package testutil

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// NewDatabase cria um banco temporário no servidor apontado por TEST_DATABASE_URL, aplica as
// migrations "up" e devolve o DSN. O banco é removido ao final do teste.
func NewDatabase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Fatal("TEST_DATABASE_URL não definida: os testes de integração exigem um PostgreSQL real")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	name := "wallet_it_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("criando banco de teste: %v", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dsn := u.String()

	_, file, _, _ := runtime.Caller(0)
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(file), "..", "..", "db", "migrations", "*.up.sql"))
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatal("nenhuma migration encontrada")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("migration %s: %v", f, err)
		}
	}
	db.Close()
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP DATABASE " + name + " WITH (FORCE)")
		admin.Close()
	})
	return dsn
}
