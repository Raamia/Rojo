-- +goose Up
CREATE TABLE events (
    id          BIGSERIAL PRIMARY KEY,
    job_id      TEXT        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    type        TEXT        NOT NULL,
    payload     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX events_job_id_idx     ON events (job_id, id);
CREATE INDEX events_created_at_idx ON events (created_at DESC);

-- +goose Down
DROP TABLE events;
