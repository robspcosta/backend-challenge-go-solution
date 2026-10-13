package repository

import (
	"context"
	"encoding/json"
	"time"

	"example.com/wagering/internal/application"
)

func (s *session) ClaimMessage(ctx context.Context, consumer, id string, hash []byte) (bool, error) {
	tag, err := s.tx.Exec(ctx, `INSERT INTO inbox_message(consumer_name,message_id,payload_hash) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, consumer, id, hash)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	var previous []byte
	if err = s.tx.QueryRow(ctx, `SELECT payload_hash FROM inbox_message WHERE consumer_name=$1 AND message_id=$2`, consumer, id).Scan(&previous); err != nil {
		return false, err
	}
	if string(previous) != string(hash) {
		return false, application.ErrConflict
	}
	return false, nil
}
func (s *session) AddEvent(ctx context.Context, event application.Event) error {
	b, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `INSERT INTO outbox_event(event_id,aggregate_id,event_type,payload,occurred_at) VALUES($1,$2,$3,$4,$5)`, event.EventID, event.AggregateID, event.EventType, b, event.OccurredAt)
	return err
}

type OutboxRow struct {
	EventID, AggregateID, EventType string
	Payload                         []byte
	Attempts                        int
}

func (r *Repository) ClaimEvents(ctx context.Context, owner string, limit int) ([]OutboxRow, error) {
	rows, err := r.pool.Query(ctx, `WITH due AS (
 SELECT event_id FROM outbox_event WHERE published_at IS NULL AND next_attempt_at<=now()
 AND (lease_until IS NULL OR lease_until<now()) ORDER BY next_attempt_at,event_id
 FOR UPDATE SKIP LOCKED LIMIT $2)
 UPDATE outbox_event o SET lease_owner=$1,lease_until=now()+interval '30 seconds',attempts=o.attempts+1
 FROM due WHERE o.event_id=due.event_id
 RETURNING o.event_id::text,o.aggregate_id::text,o.event_type,o.payload,o.attempts`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []OutboxRow{}
	for rows.Next() {
		var row OutboxRow
		if err = rows.Scan(&row.EventID, &row.AggregateID, &row.EventType, &row.Payload, &row.Attempts); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
func (r *Repository) MarkPublished(ctx context.Context, id, owner string) error {
	tag, err := r.pool.Exec(ctx, `UPDATE outbox_event SET published_at=now(),lease_owner=NULL,lease_until=NULL,last_error=NULL WHERE event_id=$1 AND lease_owner=$2 AND published_at IS NULL`, id, owner)
	if err == nil && tag.RowsAffected() != 1 {
		return application.ErrConflict
	}
	return err
}
func (r *Repository) RetryEvent(ctx context.Context, id, owner string, delay time.Duration, reason string) error {
	if len(reason) > 300 {
		reason = reason[:300]
	}
	tag, err := r.pool.Exec(ctx, `UPDATE outbox_event SET lease_owner=NULL,lease_until=NULL,next_attempt_at=now()+$3::interval,last_error=$4 WHERE event_id=$1 AND lease_owner=$2 AND published_at IS NULL`, id, owner, delay.String(), reason)
	if err == nil && tag.RowsAffected() != 1 {
		return application.ErrConflict
	}
	return err
}
