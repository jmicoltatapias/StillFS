CREATE TABLE credential_master_key_state (
    singleton SMALLINT PRIMARY KEY CHECK (singleton = 1),
    key_check BYTEA NOT NULL CHECK (octet_length(key_check) = 32),
    generation BIGINT NOT NULL CHECK (generation >= 1),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
