package repository

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"example.com/wagering/internal/application"
	"example.com/wagering/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
)

type Repository struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

type session struct{ tx pgx.Tx }

func classify(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return application.ErrNotFound
	}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "23505" {
		return errors.Join(application.ErrConflict, err)
	}
	return err
}
func (r *Repository) Within(ctx context.Context, fn func(application.Session) error) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()
	if _, err = tx.Exec(ctx, "SELECT set_config('lock_timeout','3s',true),set_config('statement_timeout','8s',true)"); err != nil {
		return err
	}
	if err = fn(&session{tx: tx}); err != nil {
		return classify(err)
	}
	return classify(tx.Commit(ctx))
}

const walletColumns = "id::text,player_id::text,currency,balance_cents,version,created_at,updated_at"

func scanWallet(row pgx.Row) (*domain.Wallet, error) {
	var id, player, currency string
	var cents, version int64
	var created, updated time.Time
	if err := row.Scan(&id, &player, &currency, &cents, &version, &created, &updated); err != nil {
		return nil, classify(err)
	}
	money, err := domain.FromCents(cents, currency)
	if err != nil {
		return nil, err
	}
	return domain.RestoreWallet(id, player, money, version, created, updated)
}
func (s *session) LockWallet(ctx context.Context, id string) (*domain.Wallet, error) {
	return scanWallet(s.tx.QueryRow(ctx, "SELECT "+walletColumns+" FROM wallet WHERE id=$1 FOR UPDATE", id))
}
func (r *Repository) Wallet(ctx context.Context, id string) (*domain.Wallet, error) {
	return scanWallet(r.pool.QueryRow(ctx, "SELECT "+walletColumns+" FROM wallet WHERE id=$1", id))
}
func (s *session) InsertWallet(ctx context.Context, w *domain.Wallet) error {
	_, err := s.tx.Exec(ctx, "INSERT INTO wallet(id,player_id,currency,balance_cents,version,created_at,updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7)", w.ID(), w.PlayerID(), w.Balance().Currency(), w.Balance().Cents(), w.Version(), w.CreatedAt(), w.UpdatedAt())
	return classify(err)
}
func (s *session) InsertOpening(ctx context.Context, w *domain.Wallet, id string) error {
	_, err := s.tx.Exec(ctx, `INSERT INTO wager_transaction(id,wallet_id,player_id,currency,kind,amount_cents,status,result_balance_cents,created_at,updated_at)
 VALUES ($1,$2,$3,$4,'OPENING',$5,'PROCESSED',$5,$6,$6)`, id, w.ID(), w.PlayerID(), w.Balance().Currency(), w.Balance().Cents(), w.CreatedAt())
	if err != nil {
		return err
	}
	zero, err := domain.Zero(w.Balance().Currency())
	if err != nil {
		return err
	}
	return s.append(ctx, id, w.ID(), w.Balance(), zero, w.Balance(), "CREDIT", w.CreatedAt())
}
func (s *session) Reserve(ctx context.Context, t *domain.Transaction) (bool, error) {
	o := t.Operation()
	hash := t.Hash()
	tag, err := s.tx.Exec(ctx, `INSERT INTO wager_transaction(id,wallet_id,player_id,currency,kind,amount_cents,provider_id,external_transaction_id,idempotency_key,payload_hash,round_id,game_id,reference_external_transaction_id,status,created_at,updated_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13,''),'PENDING',$14,$14) ON CONFLICT DO NOTHING`, t.ID(), o.WalletID, o.PlayerID, o.Money.Currency(), o.Kind, o.Money.Cents(), o.ProviderID, o.ExternalTransactionID, t.Key(), hash[:], o.RoundID, o.GameID, o.ReferenceExternalTransactionID, t.CreatedAt())
	return err == nil && tag.RowsAffected() == 1, err
}
func (s *session) Finish(ctx context.Context, t *domain.Transaction, referenceID string) error {
	tag, err := s.tx.Exec(ctx, `UPDATE wager_transaction SET status=$2,failure_code=NULLIF($3,''),result_balance_cents=$4,updated_at=$5,reference_transaction_id=NULLIF($6,'')::uuid WHERE id=$1 AND status='PENDING'`, t.ID(), t.Status(), t.FailureCode(), t.Balance().Cents(), t.UpdatedAt(), referenceID)
	if err == nil && tag.RowsAffected() != 1 {
		return application.ErrConflict
	}
	return err
}
func (s *session) SaveWallet(ctx context.Context, w *domain.Wallet, previousVersion int64) error {
	tag, err := s.tx.Exec(ctx, `UPDATE wallet SET balance_cents=$2,version=$3,updated_at=$4 WHERE id=$1 AND version=$5`, w.ID(), w.Balance().Cents(), w.Version(), w.UpdatedAt(), previousVersion)
	if err == nil && tag.RowsAffected() != 1 {
		return application.ErrConcurrencyConflict
	}
	return err
}
func (s *session) append(ctx context.Context, transaction, wallet string, amount, before, after domain.Money, direction string, at time.Time) error {
	id, err := application.NewID()
	if err != nil {
		return err
	}
	_, err = s.tx.Exec(ctx, `INSERT INTO wallet_ledger_entry(id,wallet_id,transaction_id,currency,direction,amount_cents,balance_before_cents,balance_after_cents,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, wallet, transaction, amount.Currency(), direction, amount.Cents(), before.Cents(), after.Cents(), at)
	return err
}
func (s *session) AppendLedger(ctx context.Context, t *domain.Transaction, before domain.Money, direction string) error {
	o := t.Operation()
	return s.append(ctx, t.ID(), o.WalletID, o.Money, before, t.Balance(), direction, t.UpdatedAt())
}

const transactionColumns = `id::text,status,COALESCE(result_balance_cents,0),COALESCE(failure_code,''),currency,COALESCE(provider_id,''),COALESCE(external_transaction_id,''),player_id::text,wallet_id::text,COALESCE(round_id,''),COALESCE(game_id,''),kind,amount_cents,COALESCE(reference_external_transaction_id,''),created_at,payload_hash`

func scanTransaction(row pgx.Row) (application.Stored, error) {
	var stored application.Stored
	var currency string
	var amount, balance int64
	v := &stored.View
	o := &v.Operation
	err := row.Scan(&v.TransactionID, &v.Status, &balance, &v.FailureCode, &currency, &o.ProviderID, &o.ExternalTransactionID, &o.PlayerID, &o.WalletID, &o.RoundID, &o.GameID, &o.Kind, &amount, &o.ReferenceExternalTransactionID, &v.CreatedAt, &stored.Hash)
	if err != nil {
		return stored, classify(err)
	}
	o.Money, err = domain.FromCents(amount, currency)
	if err != nil {
		return stored, err
	}
	v.Balance, err = domain.FromCents(balance, currency)
	return stored, err
}
func (s *session) FindExisting(ctx context.Context, provider, key, external string) ([]application.Stored, error) {
	rows, err := s.tx.Query(ctx, "SELECT "+transactionColumns+" FROM wager_transaction WHERE provider_id=$1 AND (idempotency_key=$2 OR external_transaction_id=$3)", provider, key, external)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []application.Stored{}
	for rows.Next() {
		value, err := scanTransaction(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}
func (s *session) Reference(ctx context.Context, provider, external string) (application.Stored, error) {
	return scanTransaction(s.tx.QueryRow(ctx, "SELECT "+transactionColumns+" FROM wager_transaction WHERE provider_id=$1 AND external_transaction_id=$2", provider, external))
}
func (r *Repository) Transaction(ctx context.Context, id string) (application.TransactionView, error) {
	stored, err := scanTransaction(r.pool.QueryRow(ctx, "SELECT "+transactionColumns+" FROM wager_transaction WHERE id=$1", id))
	return stored.View, err
}
func (r *Repository) ProviderTransaction(ctx context.Context, provider, external string) (application.TransactionView, error) {
	stored, err := scanTransaction(r.pool.QueryRow(ctx, "SELECT "+transactionColumns+" FROM wager_transaction WHERE provider_id=$1 AND external_transaction_id=$2", provider, external))
	return stored.View, err
}

type cursorData struct {
	WalletID  string    `json:"walletId"`
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
}

func (r *Repository) Ledger(ctx context.Context, id, cursor string, limit int) (application.LedgerPage, error) {
	result := application.LedgerPage{Entries: []application.LedgerEntry{}}
	if _, err := r.Wallet(ctx, id); err != nil {
		return result, err
	}
	var boundary cursorData
	if cursor != "" {
		if len(cursor) > 1024 {
			return result, application.ErrInvalidInput
		}
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return result, application.ErrInvalidInput
		}
		if err = json.Unmarshal(b, &boundary); err != nil || boundary.WalletID != id || boundary.CreatedAt.IsZero() {
			return result, application.ErrInvalidInput
		}
		if _, err = application.UUID(boundary.ID); err != nil {
			return result, err
		}
	}
	var after any
	var afterID any
	if cursor != "" {
		after = boundary.CreatedAt
		afterID = boundary.ID
	}
	rows, err := r.pool.Query(ctx, `SELECT id::text,wallet_id::text,transaction_id::text,currency,direction,amount_cents,balance_before_cents,balance_after_cents,created_at FROM wallet_ledger_entry WHERE wallet_id=$1 AND ($2::timestamptz IS NULL OR (created_at,id)>($2::timestamptz,$3::uuid)) ORDER BY created_at,id LIMIT $4`, id, after, afterID, limit+1)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry application.LedgerEntry
		var currency string
		var amount, before, after int64
		if err = rows.Scan(&entry.ID, &entry.WalletID, &entry.TransactionID, &currency, &entry.Direction, &amount, &before, &after, &entry.CreatedAt); err != nil {
			return result, err
		}
		entry.Money, err = domain.FromCents(amount, currency)
		if err != nil {
			return result, err
		}
		entry.BalanceBefore, err = domain.FromCents(before, currency)
		if err != nil {
			return result, err
		}
		entry.BalanceAfter, err = domain.FromCents(after, currency)
		if err != nil {
			return result, err
		}
		result.Entries = append(result.Entries, entry)
	}
	if err = rows.Err(); err != nil {
		return result, err
	}
	if len(result.Entries) > limit {
		result.Entries = result.Entries[:limit]
		last := result.Entries[limit-1]
		b, err := json.Marshal(cursorData{id, last.ID, last.CreatedAt})
		if err != nil {
			return result, err
		}
		result.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	return result, nil
}

var Module = fx.Module("repositories", fx.Provide(New, func(r *Repository) application.Store { return r }))
