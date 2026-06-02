ALTER TABLE agent
    ADD COLUMN source_path  TEXT,
    ADD COLUMN source_mtime TIMESTAMPTZ,
    ADD COLUMN source_kind  TEXT;

CREATE UNIQUE INDEX agent_source_path_runtime
    ON agent (runtime_id, source_path)
    WHERE source_path IS NOT NULL;

CREATE INDEX agent_runtime_source_kind
    ON agent (runtime_id, source_kind)
    WHERE source_kind IS NOT NULL;
