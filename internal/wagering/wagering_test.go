package wagering

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Cakez777/wallet-service/internal/money"
	"github.com/Cakez777/wallet-service/internal/wallet"
)

const (
	player = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"
	wlt    = "0192f291-27dd-7d3f-8071-5f8685deef37"
)

var now = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func brl(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func cmd(t *testing.T, kind Kind, amount string) Command {
	t.Helper()
	c := Command{
		ProviderID: "provider-a", ExternalTransactionID: "tx-1", IdempotencyKey: "provider-a:tx-1",
		PlayerID: player, WalletID: wlt, RoundID: "round-1", GameID: "fortune-chimp",
		Kind: kind, Money: brl(t, amount), CorrelationID: "corr-1",
	}
	if kind == KindRefund || kind == KindRollback {
		c.ReferenceExternalTransactionID = "tx-ref"
	}
	return c
}

func TestValidate(t *testing.T) {
	ok := []Command{cmd(t, KindBet, "25.00"), cmd(t, KindWin, "1.00"), cmd(t, KindLoss, "0.00"),
		cmd(t, KindRefund, "25.00"), cmd(t, KindRollback, "25.00")}
	for _, c := range ok {
		if err := c.Validate(); err != nil {
			t.Errorf("%s válido rejeitado: %v", c.Kind, err)
		}
	}

	mutate := func(kind Kind, amount string, f func(*Command)) Command {
		c := cmd(t, kind, amount)
		f(&c)
		return c
	}
	bad := map[string]Command{
		"OPENING por entrada externa": mutate(KindBet, "1.00", func(c *Command) { c.Kind = KindOpening }),
		"tipo desconhecido":           mutate(KindBet, "1.00", func(c *Command) { c.Kind = "DEPOSIT" }),
		"LOSS com valor":              cmd(t, KindLoss, "0.01"),
		"BET zero":                    cmd(t, KindBet, "0.00"),
		"WIN zero":                    cmd(t, KindWin, "0.00"),
		"REFUND zero":                 cmd(t, KindRefund, "0.00"),
		"ROLLBACK zero":               cmd(t, KindRollback, "0.00"),
		"REFUND sem referência":       mutate(KindRefund, "1.00", func(c *Command) { c.ReferenceExternalTransactionID = "" }),
		"ROLLBACK sem referência":     mutate(KindRollback, "1.00", func(c *Command) { c.ReferenceExternalTransactionID = "" }),
		"BET com referência":          mutate(KindBet, "1.00", func(c *Command) { c.ReferenceExternalTransactionID = "x" }),
		"auto-referência":             mutate(KindRefund, "1.00", func(c *Command) { c.ReferenceExternalTransactionID = c.ExternalTransactionID }),
		"playerId inválido":           mutate(KindBet, "1.00", func(c *Command) { c.PlayerID = "abc" }),
		"walletId inválido":           mutate(KindBet, "1.00", func(c *Command) { c.WalletID = "" }),
		"provedor vazio":              mutate(KindBet, "1.00", func(c *Command) { c.ProviderID = "" }),
		"chave vazia":                 mutate(KindBet, "1.00", func(c *Command) { c.IdempotencyKey = "" }),
		"rodada vazia":                mutate(KindBet, "1.00", func(c *Command) { c.RoundID = "" }),
		"jogo muito longo":            mutate(KindBet, "1.00", func(c *Command) { c.GameID = strings.Repeat("g", 256) }),
		"Money não inicializado":      mutate(KindBet, "1.00", func(c *Command) { c.Money = money.Money{} }),
	}
	for name, c := range bad {
		err := c.Validate()
		if err == nil || !IsValidationError(err) {
			t.Errorf("%s: esperado ValidationError, veio %v", name, err)
		}
	}
}

func TestPayloadHash(t *testing.T) {
	a := cmd(t, KindBet, "25.00")
	b := a
	b.IdempotencyKey = "outra-chave"
	b.CorrelationID = "outra-correlacao"
	if a.PayloadHash() != b.PayloadHash() {
		t.Error("chave de idempotência e correlationId não devem entrar no hash")
	}
	if len(a.PayloadHash()) != 64 {
		t.Errorf("hash deveria ser SHA-256 em hex, veio %q", a.PayloadHash())
	}
	changes := map[string]func(*Command){
		"valor":      func(c *Command) { c.Money = brl(t, "25.01") },
		"rodada":     func(c *Command) { c.RoundID = "round-2" },
		"jogo":       func(c *Command) { c.GameID = "x" },
		"tipo":       func(c *Command) { c.Kind = KindWin },
		"carteira":   func(c *Command) { c.WalletID = "0192f291-27dd-7d3f-8071-5f8685deef38" },
		"jogador":    func(c *Command) { c.PlayerID = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a2" },
		"provedor":   func(c *Command) { c.ProviderID = "provider-b" },
		"id externo": func(c *Command) { c.ExternalTransactionID = "tx-9" },
		"referência": func(c *Command) { c.ReferenceExternalTransactionID = "tx-0" },
	}
	for name, f := range changes {
		c := a
		f(&c)
		if c.PayloadHash() == a.PayloadHash() {
			t.Errorf("mudar %s deveria mudar o hash", name)
		}
	}
}

func ref(t *testing.T, kind Kind, amount string) *Reference {
	return &Reference{ID: "ref-id", Kind: kind, Status: StatusProcessed, PlayerID: player, WalletID: wlt, RoundID: "round-1", Money: brl(t, amount)}
}

func view(t *testing.T, balance string) WalletView {
	return WalletView{PlayerID: player, Balance: brl(t, balance)}
}

func TestPlan(t *testing.T) {
	usd, _ := money.Parse("25.00", "USD")
	type tc struct {
		name    string
		cmd     Command
		wallet  WalletView
		ref     *Reference
		action  Action
		dir     wallet.Direction
		failure FailureCode
	}
	mut := func(c Command, f func(*Command)) Command { f(&c); return c }
	withRef := func(k Kind, amount string, f func(*Reference)) *Reference {
		r := ref(t, k, amount)
		f(r)
		return r
	}
	winRef := mut(cmd(t, KindWin, "10.00"), func(c *Command) { c.ReferenceExternalTransactionID = "tx-ref" })
	lossRef := mut(cmd(t, KindLoss, "0.00"), func(c *Command) { c.ReferenceExternalTransactionID = "tx-ref" })

	cases := []tc{
		{"BET com saldo", cmd(t, KindBet, "25.00"), view(t, "100.00"), nil, ActionApply, wallet.Debit, ""},
		{"BET saldo exato", cmd(t, KindBet, "25.00"), view(t, "25.00"), nil, ActionApply, wallet.Debit, ""},
		{"BET sem saldo", cmd(t, KindBet, "25.01"), view(t, "25.00"), nil, ActionReject, "", FailInsufficientFunds},
		{"WIN sem referência", cmd(t, KindWin, "10.00"), view(t, "0.00"), nil, ActionApply, wallet.Credit, ""},
		{"WIN com BET da rodada", winRef, view(t, "0.00"), ref(t, KindBet, "5.00"), ActionApply, wallet.Credit, ""},
		{"WIN referenciando WIN", winRef, view(t, "0.00"), ref(t, KindWin, "5.00"), ActionReject, "", FailReferenceKindInvalid},
		{"WIN com referência ausente", winRef, view(t, "0.00"), nil, ActionWait, "", FailReferenceNotFound},
		{"LOSS não movimenta", cmd(t, KindLoss, "0.00"), view(t, "7.00"), nil, ActionApply, "", ""},
		{"LOSS com BET", lossRef, view(t, "7.00"), ref(t, KindBet, "5.00"), ActionApply, "", ""},
		{"REFUND de BET", cmd(t, KindRefund, "25.00"), view(t, "0.00"), ref(t, KindBet, "25.00"), ActionApply, wallet.Credit, ""},
		{"REFUND de WIN", cmd(t, KindRefund, "25.00"), view(t, "0.00"), ref(t, KindWin, "25.00"), ActionReject, "", FailReferenceKindInvalid},
		{"REFUND valor diferente", cmd(t, KindRefund, "10.00"), view(t, "0.00"), ref(t, KindBet, "25.00"), ActionReject, "", FailReversalAmountMismatch},
		{"REFUND repetido", cmd(t, KindRefund, "25.00"), view(t, "0.00"),
			withRef(KindBet, "25.00", func(r *Reference) { r.AlreadyReversed = true }), ActionReject, "", FailAlreadyReversed},
		{"ROLLBACK de BET devolve", cmd(t, KindRollback, "25.00"), view(t, "0.00"), ref(t, KindBet, "25.00"), ActionApply, wallet.Credit, ""},
		{"ROLLBACK de WIN debita", cmd(t, KindRollback, "40.00"), view(t, "40.00"), ref(t, KindWin, "40.00"), ActionApply, wallet.Debit, ""},
		{"ROLLBACK de WIN sem saldo", cmd(t, KindRollback, "40.00"), view(t, "39.99"), ref(t, KindWin, "40.00"), ActionReject, "", FailReversalInsufficientFunds},
		{"ROLLBACK de REFUND debita", cmd(t, KindRollback, "25.00"), view(t, "25.00"), ref(t, KindRefund, "25.00"), ActionApply, wallet.Debit, ""},
		{"ROLLBACK de LOSS", cmd(t, KindRollback, "0.01"), view(t, "9.00"), ref(t, KindLoss, "0.01"), ActionReject, "", FailReferenceKindInvalid},
		{"ROLLBACK de ROLLBACK", cmd(t, KindRollback, "25.00"), view(t, "9.00"), ref(t, KindRollback, "25.00"), ActionReject, "", FailReferenceKindInvalid},
		{"ROLLBACK de OPENING", cmd(t, KindRollback, "25.00"), view(t, "9.00"), ref(t, KindOpening, "25.00"), ActionReject, "", FailReferenceKindInvalid},
		{"ROLLBACK após REFUND da mesma BET", cmd(t, KindRollback, "25.00"), view(t, "0.00"),
			withRef(KindBet, "25.00", func(r *Reference) { r.AlreadyReversed = true }), ActionReject, "", FailAlreadyReversed},
		{"referência de outra rodada", cmd(t, KindRefund, "25.00"), view(t, "0.00"),
			withRef(KindBet, "25.00", func(r *Reference) { r.RoundID = "round-9" }), ActionReject, "", FailReferenceMismatch},
		{"referência de outra carteira", cmd(t, KindRefund, "25.00"), view(t, "0.00"),
			withRef(KindBet, "25.00", func(r *Reference) { r.WalletID = "outra" }), ActionReject, "", FailReferenceMismatch},
		{"referência de outro jogador", cmd(t, KindRefund, "25.00"), view(t, "0.00"),
			withRef(KindBet, "25.00", func(r *Reference) { r.PlayerID = "outro" }), ActionReject, "", FailReferenceMismatch},
		{"referência em outra moeda", cmd(t, KindRefund, "25.00"), view(t, "0.00"),
			withRef(KindBet, "25.00", func(r *Reference) { r.Money = usd }), ActionReject, "", FailReferenceMismatch},
		{"referência ausente", cmd(t, KindRefund, "25.00"), view(t, "0.00"), nil, ActionWait, "", FailReferenceNotFound},
		{"referência pendente", cmd(t, KindRefund, "25.00"), view(t, "0.00"),
			withRef(KindBet, "25.00", func(r *Reference) { r.Status = StatusPendingReference }), ActionWait, "", FailReferenceNotProcessed},
		{"referência rejeitada", cmd(t, KindRefund, "25.00"), view(t, "0.00"),
			withRef(KindBet, "25.00", func(r *Reference) { r.Status = StatusRejected }), ActionReject, "", FailReferenceNotProcessed},
		{"referência falhou", cmd(t, KindRollback, "25.00"), view(t, "0.00"),
			withRef(KindBet, "25.00", func(r *Reference) { r.Status = StatusFailed }), ActionReject, "", FailReferenceNotProcessed},
		{"carteira de outro jogador", cmd(t, KindBet, "1.00"), WalletView{PlayerID: "outro", Balance: brl(t, "9.00")}, nil, ActionReject, "", FailWalletPlayerMismatch},
		{"moeda diferente da carteira", mut(cmd(t, KindBet, "25.00"), func(c *Command) { c.Money = usd }), view(t, "99.00"), nil, ActionReject, "", FailCurrencyMismatch},
	}
	for _, c := range cases {
		out := Plan(c.cmd, c.wallet, c.ref)
		if out.Action != c.action || out.Direction != c.dir || out.Failure != c.failure {
			t.Errorf("%s: esperado {%d %q %q}, veio {%d %q %q}", c.name, c.action, c.dir, c.failure, out.Action, out.Direction, out.Failure)
		}
	}
}

func TestTransactionStateMachine(t *testing.T) {
	next := now.Add(time.Second)
	newTx := func() *Transaction {
		tx, err := NewTransaction("t1", cmd(t, KindRefund, "25.00"), now)
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}

	tx := newTx()
	if tx.Status() != StatusPending {
		t.Fatalf("estado inicial %s", tx.Status())
	}
	if err := tx.AwaitReference(next, now); err != nil || tx.Status() != StatusPendingReference {
		t.Fatalf("PENDING -> PENDING_REFERENCE: %v / %s", err, tx.Status())
	}
	if err := tx.AwaitReference(next, now); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("AwaitReference fora de PENDING deveria falhar, veio %v", err)
	}
	if err := tx.Reschedule(next.Add(time.Second), now); err != nil || tx.Attempts() != 1 {
		t.Fatalf("Reschedule: %v tentativas=%d", err, tx.Attempts())
	}
	if err := tx.MarkProcessed(brl(t, "50.00"), "ref-id", now); err != nil {
		t.Fatalf("PENDING_REFERENCE -> PROCESSED: %v", err)
	}
	if tx.ResultBalance().Amount() != "50.00" || tx.ReferenceID() != "ref-id" || tx.NextAttemptAt() != nil {
		t.Errorf("resultado inesperado após processar")
	}
	// terminal não muda mais
	if err := tx.Reject(FailAlreadyReversed, "", now); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("PROCESSED -> REJECTED deveria falhar, veio %v", err)
	}
	if err := tx.MarkProcessed(brl(t, "1.00"), "", now); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("PROCESSED -> PROCESSED deveria falhar, veio %v", err)
	}
	if err := tx.Fail("", now); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("PROCESSED -> FAILED deveria falhar, veio %v", err)
	}
	if err := tx.Reschedule(next, now); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("Reschedule em terminal deveria falhar, veio %v", err)
	}

	rej := newTx()
	if err := rej.Reject("", "", now); err == nil {
		t.Error("rejeitar sem código deveria falhar")
	}
	if err := rej.Reject(FailReferenceNotFound, "", now); err != nil || rej.FailureCode() != FailReferenceNotFound {
		t.Fatalf("Reject: %v", err)
	}
	if err := rej.AwaitReference(next, now); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("REJECTED -> PENDING_REFERENCE deveria falhar, veio %v", err)
	}

	failed := newTx()
	if err := failed.Fail("", now); err != nil || failed.FailureCode() != FailInternal || !failed.Status().IsTerminal() {
		t.Errorf("Fail: %v %s", err, failed.FailureCode())
	}
}

func TestNewTransactionRejectsInvalidCommand(t *testing.T) {
	if _, err := NewTransaction("t1", cmd(t, KindLoss, "1.00"), now); !IsValidationError(err) {
		t.Errorf("comando inválido deveria falhar, veio %v", err)
	}
	if _, err := NewTransaction("", cmd(t, KindBet, "1.00"), now); err == nil {
		t.Error("id vazio deveria falhar")
	}
}

func TestOpening(t *testing.T) {
	tx, err := NewOpening("t1", wlt, player, brl(t, "1000.00"), "corr", now)
	if err != nil {
		t.Fatal(err)
	}
	c := tx.Command()
	if tx.Origin() != OriginInternal || c.Kind != KindOpening || c.ProviderID != "" || c.ExternalTransactionID != "" || tx.PayloadHash() != "" {
		t.Errorf("OPENING não deve carregar metadados externos: %+v", c)
	}
	if _, err := NewOpening("t1", wlt, player, brl(t, "0.00"), "corr", now); err == nil {
		t.Error("OPENING com valor zero deveria falhar")
	}
}

func TestRehydrateDoesNotTransition(t *testing.T) {
	bal := brl(t, "10.00")
	tx, err := Rehydrate(Persisted{ID: "t1", Origin: OriginExternal, Command: cmd(t, KindBet, "5.00"), Status: StatusProcessed, ResultBalance: &bal, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if tx.Status() != StatusProcessed || tx.Attempts() != 0 {
		t.Errorf("reidratação alterou o estado: %s", tx.Status())
	}
	if _, err := Rehydrate(Persisted{ID: "t1", Command: cmd(t, KindBet, "5.00"), Status: "WAT"}); err == nil {
		t.Error("status desconhecido deveria falhar")
	}
}

func TestEvents(t *testing.T) {
	tx, _ := NewTransaction("t1", cmd(t, KindBet, "25.00"), now)
	if err := tx.MarkProcessed(brl(t, "975.00"), "", now); err != nil {
		t.Fatal(err)
	}
	m := wallet.Movement{Direction: wallet.Debit, Amount: brl(t, "25.00"), Before: brl(t, "1000.00"), After: brl(t, "975.00")}

	processed := NewProcessedEvent("e1", tx, now)
	balance := NewBalanceChangedEvent("e2", tx, m, 2, now)
	if processed.EventType != EventTransactionProcessed || balance.EventType != EventWalletBalanceChanged {
		t.Fatal("tipos de evento errados")
	}
	if balance.AggregateID != wlt || processed.AggregateID != "t1" {
		t.Errorf("agregados errados: %s / %s", balance.AggregateID, processed.AggregateID)
	}

	raw, _ := json.Marshal(balance)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	for _, k := range []string{"eventId", "eventType", "aggregateId", "correlationId", "occurredAt", "version", "data"} {
		if _, ok := got[k]; !ok {
			t.Errorf("envelope sem %q", k)
		}
	}
	data := got["data"].(map[string]any)
	for _, k := range []string{"walletId", "transactionId", "direction", "money", "balanceBefore", "balanceAfter", "walletVersion"} {
		if _, ok := data[k]; !ok {
			t.Errorf("WalletBalanceChanged sem %q", k)
		}
	}
	if !strings.HasSuffix(got["occurredAt"].(string), "Z") {
		t.Errorf("occurredAt deveria ser UTC: %v", got["occurredAt"])
	}

	// abertura interna: sem metadados externos nos eventos
	op, _ := NewOpening("t2", wlt, player, brl(t, "10.00"), "corr", now)
	_ = op.MarkProcessed(brl(t, "10.00"), "", now)
	raw, _ = json.Marshal(NewProcessedEvent("e3", op, now))
	for _, k := range []string{"providerId", "externalTransactionId", "roundId", "gameId"} {
		if strings.Contains(string(raw), k) {
			t.Errorf("evento de OPENING não deveria conter %q: %s", k, raw)
		}
	}
}
