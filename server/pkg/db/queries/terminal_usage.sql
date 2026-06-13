-- name: UpsertTerminalSessionUsage :exec
-- Stores the daemon-reported CUMULATIVE per-(session, model) token totals for
-- a terminal claude session. REPLACE semantics (not additive) so repeated
-- reports of the same cumulative figures — including after a daemon restart
-- re-reads the whole transcript — are idempotent.
INSERT INTO terminal_session_usage (
    session_id, workspace_id, user_id, model,
    input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, updated_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
ON CONFLICT (session_id, model)
DO UPDATE SET
    input_tokens = EXCLUDED.input_tokens,
    output_tokens = EXCLUDED.output_tokens,
    cache_read_tokens = EXCLUDED.cache_read_tokens,
    cache_write_tokens = EXCLUDED.cache_write_tokens,
    updated_at = now();

-- name: ListTerminalUsageByUser :many
-- Per-user terminal token totals for the workspace since @since (the viewer's
-- local start-of-day-N, computed in Go in the viewer's tz — same treatment as
-- the other dashboard usage reports). Powers the "Terminal usage (by user)"
-- table on the Usage page.
SELECT
    tsu.user_id,
    u.name AS user_name,
    u.email AS user_email,
    COALESCE(SUM(tsu.input_tokens), 0)::bigint       AS input_tokens,
    COALESCE(SUM(tsu.output_tokens), 0)::bigint      AS output_tokens,
    COALESCE(SUM(tsu.cache_read_tokens), 0)::bigint  AS cache_read_tokens,
    COALESCE(SUM(tsu.cache_write_tokens), 0)::bigint AS cache_write_tokens,
    COUNT(DISTINCT tsu.session_id)::int              AS session_count
FROM terminal_session_usage tsu
JOIN "user" u ON u.id = tsu.user_id
WHERE tsu.workspace_id = sqlc.arg('workspace_id')::uuid
  AND tsu.started_at >= sqlc.arg('since')::timestamptz
GROUP BY tsu.user_id, u.name, u.email
ORDER BY (SUM(tsu.input_tokens) + SUM(tsu.output_tokens) + SUM(tsu.cache_read_tokens) + SUM(tsu.cache_write_tokens)) DESC;
