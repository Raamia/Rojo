-- +goose Up
CREATE TABLE jobs (
    id          TEXT PRIMARY KEY,
    task        TEXT        NOT NULL,
    repo_path   TEXT        NOT NULL,
    status      TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX jobs_status_idx     ON jobs (status);
CREATE INDEX jobs_created_at_idx ON jobs (created_at DESC);

-- +goose Down
DROP TABLE jobs;
