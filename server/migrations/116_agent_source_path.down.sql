DROP INDEX IF EXISTS agent_runtime_source_kind;
DROP INDEX IF EXISTS agent_source_path_runtime;
ALTER TABLE agent
    DROP COLUMN IF EXISTS source_kind,
    DROP COLUMN IF EXISTS source_mtime,
    DROP COLUMN IF EXISTS source_path;
