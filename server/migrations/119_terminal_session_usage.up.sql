-- Per-session token usage for interactive terminal claude sessions, attributed
-- to the user who opened the terminal. The daemon reports cumulative per-model
-- totals (REPLACE upsert), so re-reads after a restart stay idempotent.
CREATE TABLE terminal_session_usage (
    session_id          UUID        NOT NULL,
    workspace_id        UUID        NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    user_id             UUID        NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
    model               TEXT        NOT NULL,
    input_tokens        BIGINT      NOT NULL DEFAULT 0,
    output_tokens       BIGINT      NOT NULL DEFAULT 0,
    cache_read_tokens   BIGINT      NOT NULL DEFAULT 0,
    cache_write_tokens  BIGINT      NOT NULL DEFAULT 0,
    started_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, model)
);

CREATE INDEX idx_terminal_session_usage_ws_user
    ON terminal_session_usage (workspace_id, user_id);
