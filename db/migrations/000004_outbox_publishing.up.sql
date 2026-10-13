-- Suporte ao publicador da outbox:
--  * última falha, para diagnóstico;
--  * índice que torna barata a regra "só publica o evento mais antigo de cada agregado",
--    que preserva a ordem por agregado mesmo com vários publicadores.
ALTER TABLE outbox_events ADD COLUMN last_error TEXT;

CREATE INDEX ix_outbox_aggregate_unpublished
    ON outbox_events (aggregate_id, seq)
    WHERE published_at IS NULL;
