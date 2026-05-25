package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store interface {
	Append(ctx context.Context, e Event) error
	History(ctx context.Context, jobID string) ([]Event, error)
}

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

const insertEventSQL = `
INSERT INTO events (job_id, type, payload, created_at)
VALUES ($1, $2, $3, $4)
`

func (s *PostgresStore) Append(ctx context.Context, e Event) error {
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	if _, err := s.pool.Exec(ctx, insertEventSQL, e.JobID, e.Type, payload, e.CreatedAt); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	return nil
}

const selectEventsSQL = `
SELECT job_id, type, payload, created_at
FROM events
WHERE job_id = $1
ORDER BY id ASC
`

func (s *PostgresStore) History(ctx context.Context, jobID string) ([]Event, error) {
	rows, err := s.pool.Query(ctx, selectEventsSQL, jobID)
	if err != nil {
		return nil, fmt.Errorf("select events: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var (
			e   Event
			raw []byte
		)
		if err := rows.Scan(&e.JobID, &e.Type, &raw, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &e.Payload); err != nil {
				return nil, fmt.Errorf("unmarshal payload: %w", err)
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	return out, nil
}

type PersistingBus struct {
	Inner Bus
	Store Store
}

func NewPersistingBus(inner Bus, store Store) *PersistingBus {
	return &PersistingBus{Inner: inner, Store: store}
}

func (b *PersistingBus) Publish(ctx context.Context, e Event) error {
	if err := b.Store.Append(ctx, e); err != nil {
		return err
	}
	return b.Inner.Publish(ctx, e)
}

func (b *PersistingBus) Subscribe(jobID string, buffer int) *Subscription {
	return b.Inner.Subscribe(jobID, buffer)
}

func (b *PersistingBus) Unsubscribe(sub *Subscription) {
	b.Inner.Unsubscribe(sub)
}
