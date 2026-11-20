-- +goose Up
CREATE TABLE sessions (
    id             uuid PRIMARY KEY,
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_active_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE datasets (
    id                uuid PRIMARY KEY,
    session_id        uuid NOT NULL REFERENCES sessions ON DELETE CASCADE,
    table_name        text NOT NULL,
    original_filename text NOT NULL,
    rows              bigint NOT NULL,
    columns_json      jsonb NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (session_id, table_name)
);

CREATE TABLE messages (
    id         uuid PRIMARY KEY,
    session_id uuid NOT NULL REFERENCES sessions ON DELETE CASCADE,
    role       text NOT NULL CHECK (role IN ('user', 'assistant')),
    content    text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX messages_session ON messages (session_id, created_at);

CREATE TABLE agent_runs (
    id                 uuid PRIMARY KEY,
    session_id         uuid NOT NULL REFERENCES sessions ON DELETE CASCADE,
    message_id         uuid NOT NULL REFERENCES messages ON DELETE CASCADE, -- the question
    answer_message_id  uuid REFERENCES messages ON DELETE SET NULL,
    purpose            text NOT NULL DEFAULT 'chat' CHECK (purpose IN ('chat', 'eval')),
    provider           text NOT NULL,
    model              text NOT NULL,
    status             text NOT NULL DEFAULT 'running'
                       CHECK (status IN ('running', 'done', 'error', 'cancelled', 'budget_exhausted')),
    error              text,
    steps              int NOT NULL DEFAULT 0,
    tool_seq           int NOT NULL DEFAULT 0, -- last tool_calls.seq handed out
    input_tokens       bigint NOT NULL DEFAULT 0,
    output_tokens      bigint NOT NULL DEFAULT 0,
    cached_tokens      bigint NOT NULL DEFAULT 0,
    cache_write_tokens bigint NOT NULL DEFAULT 0,
    cost_usd           numeric(12, 6) NOT NULL DEFAULT 0,
    latency_ms         int,
    created_at         timestamptz NOT NULL DEFAULT now(),
    finished_at        timestamptz
);
-- At most one running run per session; a second concurrent question fails here.
CREATE UNIQUE INDEX agent_runs_one_running ON agent_runs (session_id) WHERE status = 'running';
CREATE INDEX agent_runs_spend ON agent_runs (purpose, created_at);

CREATE TABLE tool_calls (
    id               uuid PRIMARY KEY,
    run_id           uuid NOT NULL REFERENCES agent_runs ON DELETE CASCADE,
    seq              int NOT NULL,
    tool             text NOT NULL,
    input            jsonb NOT NULL,
    output_truncated text NOT NULL,
    error            text,
    duration_ms      int NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, seq)
);

CREATE TABLE charts (
    id         uuid PRIMARY KEY,
    session_id uuid NOT NULL REFERENCES sessions ON DELETE CASCADE,
    run_id     uuid NOT NULL REFERENCES agent_runs ON DELETE CASCADE,
    path       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE charts;
DROP TABLE tool_calls;
DROP TABLE agent_runs;
DROP TABLE messages;
DROP TABLE datasets;
DROP TABLE sessions;
