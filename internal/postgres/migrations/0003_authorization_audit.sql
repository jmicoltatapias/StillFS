CREATE TABLE credential_permissions (
    access_key_id TEXT NOT NULL REFERENCES credentials(access_key_id) ON DELETE CASCADE,
    permission TEXT NOT NULL CHECK (permission IN (
        's3:ListBuckets', 's3:ListObjects', 's3:CreateBucket', 's3:DeleteBucket',
        's3:GetObject', 's3:PutObject', 's3:DeleteObject'
    )),
    granted_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (access_key_id, permission)
);

CREATE TABLE audit_events (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor TEXT NOT NULL,
    action TEXT NOT NULL,
    target TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX audit_events_occurred_id_idx ON audit_events (occurred_at DESC, id DESC);
