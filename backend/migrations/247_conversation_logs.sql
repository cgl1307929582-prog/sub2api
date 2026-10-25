-- Conversation text is stored separately from usage/billing records so retention
-- can remove content without changing historical balances or token statistics.
CREATE TABLE IF NOT EXISTS conversation_logs (
    id BIGSERIAL PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    request_id VARCHAR(96) NOT NULL,
    client_request_id VARCHAR(64) NOT NULL DEFAULT '',
    user_id BIGINT NOT NULL,
    api_key_id BIGINT NOT NULL,
    group_id BIGINT,
    model VARCHAR(200) NOT NULL DEFAULT '',
    endpoint VARCHAR(256) NOT NULL DEFAULT '',
    status_code INTEGER NOT NULL,
    stream BOOLEAN NOT NULL DEFAULT FALSE,
    user_text TEXT NOT NULL DEFAULT '',
    assistant_text TEXT NOT NULL DEFAULT '',
    error_text TEXT NOT NULL DEFAULT '',
    input_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    output_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    response_complete BOOLEAN NOT NULL DEFAULT FALSE,
    UNIQUE (request_id, api_key_id)
);
CREATE INDEX IF NOT EXISTS idx_conversation_logs_retention ON conversation_logs (created_at);
CREATE INDEX IF NOT EXISTS idx_conversation_logs_client ON conversation_logs (client_request_id, api_key_id);
