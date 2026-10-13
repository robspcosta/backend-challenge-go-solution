package repository

import (
	"context"
	"example.com/wagering/internal/application"
	"example.com/wagering/internal/domain"
	"time"
)

func (s *session) MarkPending(ctx context.Context, t *domain.Transaction, attempts int, next, expires time.Time) error {
	tag, err := s.tx.Exec(ctx, `UPDATE wager_transaction SET status='PENDING_REFERENCE',result_balance_cents=$2,updated_at=$3,reference_attempts=$4,reference_next_attempt_at=$5,reference_expires_at=$6 WHERE id=$1 AND status='PENDING'`, t.ID(), t.Balance().Cents(), t.UpdatedAt(), attempts, next, expires)
	if err == nil && tag.RowsAffected() != 1 {
		return application.ErrConflict
	}
	return err
}
func (r *Repository) DueReferences(ctx context.Context, at time.Time, limit int) ([]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT id::text FROM wager_transaction WHERE status='PENDING_REFERENCE' AND reference_next_attempt_at<=$1 ORDER BY reference_next_attempt_at,id LIMIT $2`, at, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (s *session) LockPending(ctx context.Context, id string) (*domain.Transaction, int, time.Time, error) {
	row := s.tx.QueryRow(ctx, `SELECT id::text,idempotency_key,provider_id,external_transaction_id,player_id::text,wallet_id::text,round_id,game_id,kind,amount_cents,currency,COALESCE(reference_external_transaction_id,''),payload_hash,result_balance_cents,created_at,updated_at,reference_attempts,reference_expires_at FROM wager_transaction WHERE id=$1 AND status='PENDING_REFERENCE' AND reference_next_attempt_at<=now() FOR UPDATE SKIP LOCKED`, id)
	var key, provider, external, player, wallet, round, game, kind, currency, reference string
	var hashBytes []byte
	var cents, balance int64
	var created, updated, expires time.Time
	var attempts int
	var txID string
	err := row.Scan(&txID, &key, &provider, &external, &player, &wallet, &round, &game, &kind, &cents, &currency, &reference, &hashBytes, &balance, &created, &updated, &attempts, &expires)
	if err != nil {
		return nil, 0, time.Time{}, classify(err)
	}
	money, err := domain.FromCents(cents, currency)
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	snapshot, err := domain.FromCents(balance, currency)
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	if len(hashBytes) != 32 {
		return nil, 0, time.Time{}, domain.ErrInvalidTransaction
	}
	var hash [32]byte
	copy(hash[:], hashBytes)
	operation := domain.Operation{ProviderID: provider, ExternalTransactionID: external, PlayerID: player, WalletID: wallet, RoundID: round, GameID: game, Kind: kind, Money: money, ReferenceExternalTransactionID: reference}
	saved, err := domain.RestorePending(txID, key, operation, hash, snapshot, created, updated)
	return saved, attempts, expires, err
}
func (s *session) Reschedule(ctx context.Context, id string, attempts int, next time.Time) error {
	tag, err := s.tx.Exec(ctx, `UPDATE wager_transaction SET reference_attempts=$2,reference_next_attempt_at=$3,updated_at=$4 WHERE id=$1 AND status='PENDING_REFERENCE'`, id, attempts, next, time.Now().UTC())
	if err == nil && tag.RowsAffected() != 1 {
		return application.ErrConflict
	}
	return err
}
func (s *session) ResolveReference(ctx context.Context, t *domain.Transaction, referenceID string) error {
	tag, err := s.tx.Exec(ctx, `UPDATE wager_transaction SET status=$2,failure_code=NULLIF($3,''),result_balance_cents=$4,reference_transaction_id=NULLIF($5,'')::uuid,reference_next_attempt_at=NULL,reference_expires_at=NULL,updated_at=$6 WHERE id=$1 AND status='PENDING_REFERENCE'`, t.ID(), t.Status(), t.FailureCode(), t.Balance().Cents(), referenceID, t.UpdatedAt())
	if err == nil && tag.RowsAffected() != 1 {
		return application.ErrConflict
	}
	return err
}
func (s *session) CompensationExists(ctx context.Context, referenceID string) (bool, error) {
	var exists bool
	err := s.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wager_transaction WHERE reference_transaction_id=$1 AND kind IN ('REFUND','ROLLBACK') AND status='PROCESSED')`, referenceID).Scan(&exists)
	return exists, err
}
