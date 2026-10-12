CREATE TABLE wallets (
    id         UUID PRIMARY KEY,
    player_id  UUID        NOT NULL,
    currency   CHAR(3)     NOT NULL,
    balance    BIGINT      NOT NULL CHECK (balance >= 0),
    version    BIGINT      NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (player_id, currency)
);