package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresStore is the durable production implementation of Store. It owns
// only the sidekick schema and never writes into SigNoz's telemetry database.
type PostgresStore struct{ db *sql.DB }

func NewPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	if dsn == "" {
		return nil, errors.New("session: postgres DSN is required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("session: open postgres: %w", err)
	}
	store := &PostgresStore{db: db}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("session: ping postgres: %w", err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *PostgresStore) EnsureSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS sidekick_sessions (
		channel_id TEXT NOT NULL, thread_ts TEXT NOT NULL, snapshot JSONB NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY(channel_id, thread_ts))`)
	if err != nil {
		return fmt.Errorf("session: create postgres schema: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS sidekick_audit_events (
		id TEXT PRIMARY KEY, event_type TEXT NOT NULL, correlation_id TEXT, service TEXT,
		environment TEXT, actor TEXT, occurred_at TIMESTAMPTZ NOT NULL, payload JSONB)`); err != nil {
		return fmt.Errorf("session: create audit schema: %w", err)
	}
	return nil
}

func (s *PostgresStore) Save(v View) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO sidekick_sessions(channel_id, thread_ts, snapshot) VALUES($1,$2,$3)
		ON CONFLICT(channel_id, thread_ts) DO UPDATE SET snapshot=EXCLUDED.snapshot, updated_at=NOW()`, v.ChannelID, v.ThreadTS, b)
	return err
}
func (s *PostgresStore) Load(channelID, threadTS string) (View, error) {
	var b []byte
	err := s.db.QueryRow(`SELECT snapshot FROM sidekick_sessions WHERE channel_id=$1 AND thread_ts=$2`, channelID, threadTS).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return View{}, ErrNotFound
	}
	if err != nil {
		return View{}, err
	}
	var v View
	if err := json.Unmarshal(b, &v); err != nil {
		return View{}, err
	}
	return v, nil
}
func (s *PostgresStore) Delete(channelID, threadTS string) error {
	_, err := s.db.Exec(`DELETE FROM sidekick_sessions WHERE channel_id=$1 AND thread_ts=$2`, channelID, threadTS)
	return err
}
func (s *PostgresStore) Close() error { return s.db.Close() }

type PostgresAuditor struct{ db *sql.DB }

func (s *PostgresStore) Auditor() *PostgresAuditor { return &PostgresAuditor{db: s.db} }
func (a *PostgresAuditor) Append(event AuditEvent) error {
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	if event.ID == "" {
		event.ID = fmt.Sprintf("%d", event.At.UnixNano())
	}
	payload := event.Payload
	if payload == "" {
		payload = "{}"
	}
	_, err := a.db.Exec(`INSERT INTO sidekick_audit_events(id,event_type,correlation_id,service,environment,actor,occurred_at,payload) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(id) DO NOTHING`, event.ID, event.Type, event.CorrelationID, event.Service, event.Environment, event.Actor, event.At, []byte(payload))
	return err
}
