CREATE TABLE transactions (
    id UUID PRIMARY KEY,
    wallet_id UUID NOT NULL REFERENCES wallets(id) ON DELETE CASCADE,
    type VARCHAR(50) NOT NULL, -- Ex: DEPOSIT, WITHDRAWAL
    amount BIGINT NOT NULL,    -- Valor em centavos
    currency CHAR(3) NOT NULL,
    idempotency_key VARCHAR(255) UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_transactions_wallet_id ON transactions(wallet_id);
CREATE INDEX idx_transactions_idempotency_key ON transactions(idempotency_key);