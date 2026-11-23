CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS feedback_tickets (
    id BIGSERIAL PRIMARY KEY,
    user_message TEXT NOT NULL,
    technical_summary TEXT NOT NULL DEFAULT '',
    embedding VECTOR(768),
    app_commit_sha TEXT NOT NULL DEFAULT '',
    module_tag TEXT NOT NULL DEFAULT 'general',
    occurrence_count INTEGER NOT NULL DEFAULT 1,
    priority TEXT NOT NULL DEFAULT 'MEDIUM'
        CHECK (priority IN ('LOW', 'MEDIUM', 'NORMAL', 'HIGH', 'CRITICAL')),
    status TEXT NOT NULL DEFAULT 'QUEUED'
        CHECK (status IN ('QUEUED', 'RUNNING', 'PR_OPENED', 'MERGED', 'ABORTED', 'IGNORED_OUTDATED', 'FEATURE_REQUEST')),
    max_diff_override INTEGER,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_feedback_tickets_embedding
    ON feedback_tickets
    USING hnsw (embedding vector_cosine_ops);

CREATE INDEX IF NOT EXISTS idx_feedback_tickets_status_module
    ON feedback_tickets (status, module_tag);

CREATE INDEX IF NOT EXISTS idx_feedback_tickets_created_at
    ON feedback_tickets (created_at);

CREATE TABLE IF NOT EXISTS ticket_telemetry (
    id BIGSERIAL PRIMARY KEY,
    ticket_id BIGINT NOT NULL REFERENCES feedback_tickets(id) ON DELETE CASCADE,
    breadcrumbs JSONB NOT NULL DEFAULT '[]'::jsonb,
    console_errors JSONB NOT NULL DEFAULT '[]'::jsonb,
    network_logs JSONB NOT NULL DEFAULT '[]'::jsonb,
    device_meta JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ticket_telemetry_ticket_id
    ON ticket_telemetry (ticket_id);

CREATE TABLE IF NOT EXISTS agent_runs (
    id BIGSERIAL PRIMARY KEY,
    ticket_id BIGINT NOT NULL REFERENCES feedback_tickets(id) ON DELETE CASCADE,
    attempt_number INTEGER NOT NULL DEFAULT 1,
    modified_files TEXT[] NOT NULL DEFAULT '{}',
    test_output TEXT NOT NULL DEFAULT '',
    guardrail_status TEXT NOT NULL
        CHECK (guardrail_status IN (
            'PASSED',
            'BLOCKED_PROTECTED_PATH',
            'BLOCKED_MAX_DIFF',
            'BLOCKED_REVIEWER',
            'TEST_FAILED',
            'RUNTIME_ERROR',
            'PROMPT_INJECTION_DETECTED'
        )),
    pr_number INTEGER,
    pr_url TEXT NOT NULL DEFAULT '',
    human_review_note TEXT NOT NULL DEFAULT '',
    tokens_used INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agent_runs_ticket_id
    ON agent_runs (ticket_id);

CREATE INDEX IF NOT EXISTS idx_agent_runs_pr_number
    ON agent_runs (pr_number);
