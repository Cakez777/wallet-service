//go:build integration

package app

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cakez777/wallet-service/internal/wagering"
)

// Estes testes disparam PROCESSOS DO SISTEMA OPERACIONAL independentes (o próprio binário de
// teste reexecutado), cada um com seu pool de conexões, competindo pela mesma carteira no
// mesmo instante. É o cenário "três processos" do desafio: nada é compartilhado em memória,
// só o PostgreSQL.

const helperModeEnv = "WALLET_HELPER_MODE"

type childResult struct {
	status, failure string
	replay          bool
}

// TestHelperProcess é o corpo dos processos filhos. Fora do modo auxiliar, não faz nada.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		t.Skip("processo auxiliar: só roda quando disparado pelos testes multiprocesso")
	}
	db, err := sql.Open("pgx", os.Getenv("HELPER_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := NewService(db, Config{})
	f := fixture{walletID: os.Getenv("HELPER_WALLET"), playerID: os.Getenv("HELPER_PLAYER")}
	idx := os.Getenv("HELPER_INDEX")
	startNs, _ := strconv.ParseInt(os.Getenv("HELPER_START_NS"), 10, 64)

	var cmds []wagering.Command
	switch mode {
	case "two-bets":
		cmds = append(cmds,
			f.cmd(t, wagering.KindBet, "bet-"+idx+"-a", "80.00"),
			f.cmd(t, wagering.KindBet, "bet-"+idx+"-b", "80.00"))
	case "same-key":
		for i := 0; i < 20; i++ {
			cmds = append(cmds, f.cmd(t, wagering.KindBet, "bet-same", "50.00"))
		}
	default:
		t.Fatalf("modo desconhecido %q", mode)
	}

	time.Sleep(time.Until(time.Unix(0, startNs))) // largada sincronizada
	var wg sync.WaitGroup
	lines := make([]string, len(cmds))
	for i, c := range cmds {
		wg.Add(1)
		go func(i int, c wagering.Command) {
			defer wg.Done()
			r, err := svc.Process(context.Background(), c)
			if err != nil {
				lines[i] = "ERROR " + strings.ReplaceAll(err.Error(), "\n", " ")
				return
			}
			lines[i] = fmt.Sprintf("RESULT %s %s %t", r.Transaction.Status(), r.Transaction.FailureCode(), r.Replay)
		}(i, c)
	}
	wg.Wait()
	for _, l := range lines {
		fmt.Println(l)
	}
}

func (e *env) runChildren(mode string, n int, f fixture) []childResult {
	e.t.Helper()
	start := time.Now().Add(2 * time.Second)
	type out struct {
		stdout, stderr bytes.Buffer
		err            error
	}
	outs := make([]*out, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		o := &out{}
		outs[i] = o
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(),
			helperModeEnv+"="+mode, "HELPER_DSN="+e.dsn, "HELPER_WALLET="+f.walletID, "HELPER_PLAYER="+f.playerID,
			"HELPER_INDEX="+strconv.Itoa(i), "HELPER_START_NS="+strconv.FormatInt(start.UnixNano(), 10))
		cmd.Stdout, cmd.Stderr = &o.stdout, &o.stderr
		wg.Add(1)
		go func() { defer wg.Done(); o.err = cmd.Run() }()
	}
	wg.Wait()

	var all []childResult
	for i, o := range outs {
		if o.err != nil {
			e.t.Fatalf("processo filho %d falhou: %v\nstdout:\n%s\nstderr:\n%s", i, o.err, o.stdout.String(), o.stderr.String())
		}
		sc := bufio.NewScanner(&o.stdout)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "ERROR ") {
				e.t.Fatalf("processo filho %d: %s", i, line)
			}
			if !strings.HasPrefix(line, "RESULT ") {
				continue
			}
			p := strings.Split(line, " ")
			// "RESULT <status> <failure> <replay>"; failure pode estar vazio -> 3 campos úteis
			r := childResult{status: p[1]}
			if len(p) == 4 {
				r.failure = p[2]
				r.replay = p[3] == "true"
			} else {
				r.replay = p[len(p)-1] == "true"
			}
			all = append(all, r)
		}
	}
	return all
}

func TestThreeProcessesRaceTwoBetsOf80On100(t *testing.T) {
	e := newEnv(t)
	svc := e.services(1, Config{})[0]
	for round := 0; round < 5; round++ {
		f := e.wallet(svc, "100.00")
		res := e.runChildren("two-bets", 3, f) // 3 processos x 2 apostas de 80.00 = 6 apostas simultâneas
		if len(res) != 6 {
			t.Fatalf("rodada %d: %d resultados, esperado 6", round, len(res))
		}
		processed, rejected := 0, 0
		for _, r := range res {
			switch r.status {
			case string(wagering.StatusProcessed):
				processed++
			case string(wagering.StatusRejected):
				rejected++
				if r.failure != string(wagering.FailInsufficientFunds) {
					t.Errorf("código da rejeição: %q", r.failure)
				}
			default:
				t.Errorf("status inesperado: %+v", r)
			}
		}
		if processed != 1 || rejected != 5 {
			t.Fatalf("rodada %d: %d processadas, %d rejeitadas (esperado 1 e 5)", round, processed, rejected)
		}
		if e.balance(f.walletID) != "20.00" || e.ledgerCount(f.walletID, "DEBIT") != 1 {
			t.Fatalf("rodada %d: saldo %s, débitos %d", round, e.balance(f.walletID), e.ledgerCount(f.walletID, "DEBIT"))
		}
		e.assertInvariants(svc, f.walletID)
	}
}

func TestThreeProcessesSendTheSameIdempotentBet(t *testing.T) {
	e := newEnv(t)
	svc := e.services(1, Config{})[0]
	f := e.wallet(svc, "100.00")
	res := e.runChildren("same-key", 3, f) // 60 envios idênticos vindos de 3 processos
	if len(res) != 60 {
		t.Fatalf("%d resultados, esperado 60", len(res))
	}
	originals := 0
	for _, r := range res {
		if r.status != string(wagering.StatusProcessed) {
			t.Fatalf("todas as respostas devem ser PROCESSED: %+v", r)
		}
		if !r.replay {
			originals++
		}
	}
	if originals != 1 {
		t.Errorf("%d respostas originais, esperado exatamente 1 (as outras 59 são replays)", originals)
	}
	if e.balance(f.walletID) != "50.00" || e.ledgerCount(f.walletID, "DEBIT") != 1 {
		t.Fatalf("saldo %s, débitos %d", e.balance(f.walletID), e.ledgerCount(f.walletID, "DEBIT"))
	}
	e.assertInvariants(svc, f.walletID)
}
