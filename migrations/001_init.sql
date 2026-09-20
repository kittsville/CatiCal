CREATE TABLE IF NOT EXISTS feeds (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL,
    prefix_summaries BOOLEAN NOT NULL DEFAULT FALSE,
    feed_token_salt BYTEA NOT NULL,
    feed_token_hash BYTEA NOT NULL,
    manage_token_salt BYTEA NOT NULL,
    manage_token_hash BYTEA NOT NULL,
    merged_ics BYTEA,
    merged_at TIMESTAMPTZ,
    last_request_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sources (
    id UUID PRIMARY KEY,
    feed_id UUID NOT NULL REFERENCES feeds (id) ON DELETE CASCADE,
    url TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    position INTEGER NOT NULL,
    last_ics BYTEA,
    last_success_at TIMESTAMPTZ,
    last_error TEXT,
    last_attempt_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS sources_feed_id_position_idx ON sources (feed_id, position);
