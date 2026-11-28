-- name: CreateSession :one
INSERT INTO sessions (id) VALUES ($1) RETURNING *;

-- name: GetSession :one
SELECT * FROM sessions WHERE id = $1;

-- name: TouchSession :exec
UPDATE sessions SET last_active_at = now() WHERE id = $1;

-- name: ListExpiredSessions :many
SELECT id FROM sessions WHERE last_active_at < $1;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = $1;

-- name: CreateDataset :one
INSERT INTO datasets (id, session_id, table_name, original_filename, rows, columns_json)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: ListDatasets :many
SELECT * FROM datasets WHERE session_id = $1 ORDER BY created_at;

-- name: CreateMessage :one
INSERT INTO messages (id, session_id, role, content) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: ListMessages :many
SELECT * FROM messages WHERE session_id = $1 ORDER BY created_at, id;

-- name: CreateRun :one
INSERT INTO agent_runs (id, session_id, message_id, purpose, provider, model)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetRun :one
SELECT * FROM agent_runs WHERE id = $1;

-- name: FinishRun :exec
UPDATE agent_runs SET
    status = $2, error = $3, answer_message_id = $4, steps = $5,
    input_tokens = $6, output_tokens = $7, cached_tokens = $8, cache_write_tokens = $9,
    cost_usd = $10, latency_ms = $11, finished_at = now()
WHERE id = $1;

-- name: NextToolSeq :one
UPDATE agent_runs SET tool_seq = tool_seq + 1 WHERE id = $1 RETURNING tool_seq;

-- name: FailStaleRuns :exec
UPDATE agent_runs SET status = 'error', error = 'service restarted during run', finished_at = now()
WHERE status = 'running';

-- name: SpentSince :one
SELECT COALESCE(SUM(cost_usd), 0)::float8 AS spent
FROM agent_runs WHERE purpose = 'chat' AND created_at >= $1;

-- name: CreateToolCall :exec
INSERT INTO tool_calls (id, run_id, seq, tool, input, output_truncated, error, duration_ms)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListToolCallsForRun :many
SELECT * FROM tool_calls WHERE run_id = $1 ORDER BY seq;

-- name: ListFinishedTurns :many
-- Finished question/answer pairs, newest first; callers reverse them.
SELECT r.id AS run_id, q.content AS question, a.content AS answer
FROM agent_runs r
JOIN messages q ON q.id = r.message_id
JOIN messages a ON a.id = r.answer_message_id
WHERE r.session_id = $1 AND r.status IN ('done', 'budget_exhausted')
ORDER BY r.created_at DESC
LIMIT $2;

-- name: CreateChart :exec
INSERT INTO charts (id, session_id, run_id, path) VALUES ($1, $2, $3, $4);

-- name: GetChart :one
SELECT * FROM charts WHERE id = $1;
