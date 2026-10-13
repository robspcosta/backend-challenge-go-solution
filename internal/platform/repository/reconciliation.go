package repository

import (
	"context"
	"errors"
	"math/big"
	"time"

	"example.com/wagering/internal/application"
	"example.com/wagering/internal/domain"
	"github.com/jackc/pgx/v5"
)

func (r *Repository) Reconcile(ctx context.Context, walletID string) (application.Reconciliation, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return application.Reconciliation{}, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	var currency string
	var stored int64
	if err = tx.QueryRow(ctx, `SELECT currency,balance_cents FROM wallet WHERE id=$1`, walletID).Scan(&currency, &stored); err != nil {
		return application.Reconciliation{}, classify(err)
	}
	var reconstructed string
	var count int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(CASE direction WHEN 'CREDIT' THEN amount_cents::numeric ELSE -amount_cents::numeric END),0)::text,COUNT(*) FROM wallet_ledger_entry WHERE wallet_id=$1`, walletID).Scan(&reconstructed, &count); err != nil {
		return application.Reconciliation{}, err
	}
	calculated, ok := new(big.Int).SetString(reconstructed, 10)
	if !ok || !calculated.IsInt64() {
		return application.Reconciliation{}, errors.Join(application.ErrUnavailable, errors.New("reconstructed balance exceeds Money range"))
	}
	difference := new(big.Int).Sub(big.NewInt(stored), calculated)
	if !difference.IsInt64() {
		return application.Reconciliation{}, errors.Join(application.ErrUnavailable, errors.New("reconciliation difference exceeds Money range"))
	}
	storedMoney, err := domain.FromCents(stored, currency)
	if err != nil {
		return application.Reconciliation{}, err
	}
	calculatedMoney, err := domain.FromCents(calculated.Int64(), currency)
	if err != nil {
		return application.Reconciliation{}, err
	}
	differenceMoney, err := domain.FromCents(difference.Int64(), currency)
	if err != nil {
		return application.Reconciliation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return application.Reconciliation{}, err
	}
	return application.Reconciliation{WalletID: walletID, StoredBalance: storedMoney, CalculatedBalance: calculatedMoney, Difference: differenceMoney, Consistent: difference.Sign() == 0, CheckedEntries: count}, nil
}

func (r *Repository) OutboxLag(ctx context.Context) (float64, error) {
	var seconds float64
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(EXTRACT(EPOCH FROM now()-MIN(occurred_at)),0)::float8 FROM outbox_event WHERE published_at IS NULL`).Scan(&seconds)
	return seconds, err
}
