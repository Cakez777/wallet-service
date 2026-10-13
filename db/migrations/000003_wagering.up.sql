-- Substitui o modelo provisório de depósito/saque pelo domínio de apostas.
DROP TABLE IF EXISTS transactions;

-- ---------------------------------------------------------------------------
-- wager_transactions: operações externas (BET/WIN/LOSS/REFUND/ROLLBACK) e a
-- abertura interna de carteira (OPENING).
-- ---------------------------------------------------------------------------
CREATE TABLE wager_transactions (
    id                                UUID        PRIMARY KEY,
    origin                            TEXT        NOT NULL CHECK (origin IN ('EXTERNAL', 'INTERNAL')),
    kind                              TEXT        NOT NULL CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    status                            TEXT        NOT NULL CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),
    wallet_id                         UUID        NOT NULL REFERENCES wallets (id),
    player_id                         UUID        NOT NULL,
    amount                            BIGINT      NOT NULL CHECK (amount >= 0),
    currency                          CHAR(3)     NOT NULL,
    provider_id                       TEXT,
    external_transaction_id           TEXT,
    idempotency_key                   TEXT,
    payload_hash                      TEXT,
    round_id                          TEXT,
    game_id                           TEXT,
    reference_external_transaction_id TEXT,
    reference_transaction_id          UUID        REFERENCES wager_transactions (id),
    failure_code                      TEXT,
    result_balance_after              BIGINT      CHECK (result_balance_after >= 0),
    correlation_id                    TEXT        NOT NULL,
    attempts                          INT         NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at                   TIMESTAMPTZ,
    created_at                        TIMESTAMPTZ NOT NULL,
    updated_at                        TIMESTAMPTZ NOT NULL,

    -- Operações internas e externas têm formatos distintos.
    CONSTRAINT wager_origin_shape CHECK (
        (origin = 'EXTERNAL' AND kind <> 'OPENING'
            AND provider_id IS NOT NULL AND external_transaction_id IS NOT NULL
            AND idempotency_key IS NOT NULL AND payload_hash IS NOT NULL
            AND round_id IS NOT NULL AND game_id IS NOT NULL)
        OR
        (origin = 'INTERNAL' AND kind = 'OPENING'
            AND provider_id IS NULL AND external_transaction_id IS NULL
            AND idempotency_key IS NULL AND payload_hash IS NULL
            AND round_id IS NULL AND game_id IS NULL
            AND reference_external_transaction_id IS NULL)
    ),
    CONSTRAINT wager_amount_by_kind CHECK (
        (kind = 'LOSS' AND amount = 0)
        OR (kind IN ('OPENING', 'BET', 'WIN', 'REFUND', 'ROLLBACK') AND amount > 0)
    ),
    CONSTRAINT wager_reversal_has_reference CHECK (
        kind NOT IN ('REFUND', 'ROLLBACK') OR reference_external_transaction_id IS NOT NULL
    ),
    CONSTRAINT wager_failure_code_shape CHECK (
        (status IN ('REJECTED', 'FAILED')) = (failure_code IS NOT NULL)
    ),
    CONSTRAINT wager_processed_has_balance CHECK (
        status <> 'PROCESSED' OR result_balance_after IS NOT NULL
    ),
    CONSTRAINT wager_pending_has_schedule CHECK (
        status <> 'PENDING_REFERENCE' OR next_attempt_at IS NOT NULL
    )
);

-- Idempotência persistente: mesma chave ou mesma operação externa nunca entram duas vezes.
CREATE UNIQUE INDEX ux_wager_provider_idempotency
    ON wager_transactions (provider_id, idempotency_key) WHERE origin = 'EXTERNAL';
CREATE UNIQUE INDEX ux_wager_provider_external
    ON wager_transactions (provider_id, external_transaction_id) WHERE origin = 'EXTERNAL';
-- Crédito inicial nunca duplicado.
CREATE UNIQUE INDEX ux_wager_one_opening_per_wallet
    ON wager_transactions (wallet_id) WHERE kind = 'OPENING';
-- Uma transação só recebe uma reversão (REFUND ou ROLLBACK) bem-sucedida.
CREATE UNIQUE INDEX ux_wager_one_reversal_per_reference
    ON wager_transactions (reference_transaction_id)
    WHERE kind IN ('REFUND', 'ROLLBACK') AND status = 'PROCESSED';
CREATE INDEX ix_wager_wallet ON wager_transactions (wallet_id);
CREATE INDEX ix_wager_pending ON wager_transactions (next_attempt_at) WHERE status = 'PENDING_REFERENCE';

-- Estados terminais e colunas de identidade são imutáveis; nada é apagado.
CREATE FUNCTION wager_transactions_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' OR TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'wager_transactions is append-only (% not allowed)', TG_OP
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'wager transaction % is terminal (%) and cannot change', OLD.id, OLD.status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'PENDING_REFERENCE' AND NEW.status = 'PENDING' THEN
        RAISE EXCEPTION 'invalid transition PENDING_REFERENCE -> PENDING'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.id <> OLD.id OR NEW.origin <> OLD.origin OR NEW.kind <> OLD.kind
       OR NEW.wallet_id <> OLD.wallet_id OR NEW.player_id <> OLD.player_id
       OR NEW.amount <> OLD.amount OR NEW.currency <> OLD.currency
       OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
       OR NEW.external_transaction_id IS DISTINCT FROM OLD.external_transaction_id
       OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
       OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
       OR NEW.round_id IS DISTINCT FROM OLD.round_id
       OR NEW.game_id IS DISTINCT FROM OLD.game_id
       OR NEW.reference_external_transaction_id IS DISTINCT FROM OLD.reference_external_transaction_id
       OR NEW.correlation_id <> OLD.correlation_id
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'identity columns of wager transaction % are immutable', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER wager_transactions_no_update
    BEFORE UPDATE ON wager_transactions FOR EACH ROW EXECUTE FUNCTION wager_transactions_guard();
CREATE TRIGGER wager_transactions_no_delete
    BEFORE DELETE ON wager_transactions FOR EACH ROW EXECUTE FUNCTION wager_transactions_guard();
CREATE TRIGGER wager_transactions_no_truncate
    BEFORE TRUNCATE ON wager_transactions FOR EACH STATEMENT EXECUTE FUNCTION wager_transactions_guard();

-- ---------------------------------------------------------------------------
-- wallet_ledger_entries: ledger append-only.
-- ---------------------------------------------------------------------------
CREATE TABLE wallet_ledger_entries (
    seq            BIGINT      GENERATED ALWAYS AS IDENTITY,
    id             UUID        PRIMARY KEY,
    wallet_id      UUID        NOT NULL REFERENCES wallets (id),
    transaction_id UUID        NOT NULL REFERENCES wager_transactions (id),
    direction      TEXT        NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    amount         BIGINT      NOT NULL CHECK (amount > 0),
    currency       CHAR(3)     NOT NULL,
    balance_before BIGINT      NOT NULL CHECK (balance_before >= 0),
    balance_after  BIGINT      NOT NULL CHECK (balance_after >= 0),
    created_at     TIMESTAMPTZ NOT NULL,
    CONSTRAINT ledger_balance_math CHECK (
        (direction = 'CREDIT' AND balance_after = balance_before + amount)
        OR (direction = 'DEBIT' AND balance_after = balance_before - amount)
    ),
    CONSTRAINT ux_ledger_wallet_transaction UNIQUE (wallet_id, transaction_id)
);
CREATE UNIQUE INDEX ux_ledger_wallet_seq ON wallet_ledger_entries (wallet_id, seq);

CREATE FUNCTION ledger_forbid_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'wallet_ledger_entries is append-only (% not allowed)', TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END $$;

CREATE TRIGGER ledger_no_update_delete
    BEFORE UPDATE OR DELETE ON wallet_ledger_entries FOR EACH ROW EXECUTE FUNCTION ledger_forbid_mutation();
CREATE TRIGGER ledger_no_truncate
    BEFORE TRUNCATE ON wallet_ledger_entries FOR EACH STATEMENT EXECUTE FUNCTION ledger_forbid_mutation();

-- O saldo da carteira só pode ser confirmado junto com o lançamento que o justifica:
-- no commit, wallets.balance precisa ser igual ao balance_after do último lançamento.
CREATE FUNCTION wallets_check_ledger() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    last_after BIGINT;
BEGIN
    SELECT balance_after INTO last_after
      FROM wallet_ledger_entries
     WHERE wallet_id = NEW.id
     ORDER BY seq DESC
     LIMIT 1;
    IF NOT FOUND THEN
        last_after := 0;
    END IF;
    IF NEW.balance <> last_after THEN
        RAISE EXCEPTION 'wallet % balance % does not match ledger balance %', NEW.id, NEW.balance, last_after
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER wallets_balance_matches_ledger
    AFTER INSERT OR UPDATE OF balance ON wallets
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wallets_check_ledger();

-- ---------------------------------------------------------------------------
-- outbox_events e inbox_messages
-- ---------------------------------------------------------------------------
CREATE TABLE outbox_events (
    seq             BIGINT      GENERATED ALWAYS AS IDENTITY,
    event_id        UUID        PRIMARY KEY,
    aggregate_id    UUID        NOT NULL,
    aggregate_type  TEXT        NOT NULL,
    event_type      TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_by       TEXT,
    locked_until    TIMESTAMPTZ,
    published_at    TIMESTAMPTZ
);
CREATE INDEX ix_outbox_unpublished ON outbox_events (next_attempt_at) WHERE published_at IS NULL;

CREATE TABLE inbox_messages (
    consumer_name TEXT        NOT NULL,
    message_id    TEXT        NOT NULL,
    payload_hash  TEXT        NOT NULL,
    received_at   TIMESTAMPTZ NOT NULL,
    completed_at  TIMESTAMPTZ,
    PRIMARY KEY (consumer_name, message_id)
);
