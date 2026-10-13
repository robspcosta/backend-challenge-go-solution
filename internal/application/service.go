package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"example.com/wagering/internal/domain"
)

var ErrInvalidInput = errors.New("invalid input")
var ErrConflict = errors.New("conflict")
var ErrConcurrencyConflict = errors.Join(ErrConflict, errors.New("concurrent wallet update"))
var ErrNotFound = errors.New("not found")
var ErrUnavailable = errors.New("temporarily unavailable")

type WalletView struct {
	ID       string       `json:"id"`
	PlayerID string       `json:"playerId"`
	Balance  domain.Money `json:"balance"`
	Version  int64        `json:"version"`
}

func View(w *domain.Wallet) WalletView {
	return WalletView{w.ID(), w.PlayerID(), w.Balance(), w.Version()}
}

type Result struct {
	TransactionID    string       `json:"transactionId"`
	Status           string       `json:"status"`
	Balance          domain.Money `json:"balance"`
	FailureCode      string       `json:"failureCode,omitempty"`
	IdempotentReplay bool         `json:"idempotentReplay"`
}
type TransactionView struct {
	Result
	Operation domain.Operation `json:"operation"`
	CreatedAt time.Time        `json:"createdAt"`
}
type Stored struct {
	View TransactionView
	Hash []byte
}
type LedgerEntry struct {
	ID            string       `json:"id"`
	WalletID      string       `json:"walletId"`
	TransactionID string       `json:"transactionId"`
	Direction     string       `json:"direction"`
	Money         domain.Money `json:"money"`
	BalanceBefore domain.Money `json:"balanceBefore"`
	BalanceAfter  domain.Money `json:"balanceAfter"`
	CreatedAt     time.Time    `json:"createdAt"`
}
type LedgerPage struct {
	Entries    []LedgerEntry `json:"entries"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

// Session confines all repository operations to the same SQL transaction.
type Session interface {
	ClaimMessage(context.Context, string, string, []byte) (bool, error)
	AddEvent(context.Context, Event) error
	FindExisting(context.Context, string, string, string) ([]Stored, error)
	LockWallet(context.Context, string) (*domain.Wallet, error)
	InsertWallet(context.Context, *domain.Wallet) error
	InsertOpening(context.Context, *domain.Wallet, string) error
	Reserve(context.Context, *domain.Transaction) (bool, error)
	Finish(context.Context, *domain.Transaction, string) error
	SaveWallet(context.Context, *domain.Wallet, int64) error
	AppendLedger(context.Context, *domain.Transaction, domain.Money, string) error
	Reference(context.Context, string, string) (Stored, error)
	MarkPending(context.Context, *domain.Transaction, int, time.Time, time.Time) error
	LockPending(context.Context, string) (*domain.Transaction, int, time.Time, error)
	Reschedule(context.Context, string, int, time.Time) error
	ResolveReference(context.Context, *domain.Transaction, string) error
	CompensationExists(context.Context, string) (bool, error)
}
type Store interface {
	Within(context.Context, func(Session) error) error
	Wallet(context.Context, string) (*domain.Wallet, error)
	Transaction(context.Context, string) (TransactionView, error)
	ProviderTransaction(context.Context, string, string) (TransactionView, error)
	Ledger(context.Context, string, string, int) (LedgerPage, error)
	DueReferences(context.Context, time.Time, int) ([]string, error)
	Reconcile(context.Context, string) (Reconciliation, error)
}
type Reconciliation struct {
	WalletID          string       `json:"walletId"`
	StoredBalance     domain.Money `json:"storedBalance"`
	CalculatedBalance domain.Money `json:"calculatedBalance"`
	Difference        domain.Money `json:"difference"`
	Consistent        bool         `json:"consistent"`
	CheckedEntries    int64        `json:"checkedEntries"`
}
type Metrics interface {
	ProcessResult(string, bool, time.Duration)
	ConcurrencyConflict()
	ReconciliationDivergence()
}
type Service struct {
	store   Store
	metrics Metrics
}

func New(store Store) *Service { return &Service{store: store} }
func NewWithMetrics(store Store, metrics Metrics) *Service {
	return &Service{store: store, metrics: metrics}
}
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}
func UUID(s string) (string, error) {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return "", ErrInvalidInput
	}
	compact := strings.ReplaceAll(s, "-", "")
	if len(compact) != 32 {
		return "", ErrInvalidInput
	}
	b, err := hex.DecodeString(compact)
	if err != nil {
		return "", ErrInvalidInput
	}
	allZero := true
	for _, v := range b {
		if v != 0 {
			allZero = false
		}
	}
	if allZero {
		return "", ErrInvalidInput
	}
	return strings.ToLower(s), nil
}

// CanonicalPayload sorts all object keys using encoding/json maps. Every business value is a string.
func CanonicalPayload(o domain.Operation) ([]byte, [32]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, [32]byte{}, err
	}
	payload := map[string]any{"providerId": o.ProviderID, "externalTransactionId": o.ExternalTransactionID, "playerId": o.PlayerID, "walletId": o.WalletID, "roundId": o.RoundID, "gameId": o.GameID, "kind": o.Kind, "money": map[string]string{"amount": o.Money.Amount(), "currency": o.Money.Currency()}}
	if o.ReferenceExternalTransactionID != "" {
		payload["referenceExternalTransactionId"] = o.ReferenceExternalTransactionID
	}
	b, err := json.Marshal(payload)
	return b, sha256.Sum256(b), err
}
func (s *Service) CreateWallet(ctx context.Context, playerID string, balance domain.Money) (WalletView, error) {
	playerID, err := UUID(playerID)
	if err != nil || !balance.Valid() || balance.Cents() < 0 {
		return WalletView{}, ErrInvalidInput
	}
	id, err := NewID()
	if err != nil {
		return WalletView{}, err
	}
	now := time.Now().UTC()
	w, err := domain.NewWallet(id, playerID, balance, now)
	if err != nil {
		return WalletView{}, err
	}
	err = s.store.Within(ctx, func(tx Session) error {
		if err := tx.InsertWallet(ctx, w); err != nil {
			return err
		}
		if balance.Cents() > 0 {
			openingID, err := NewID()
			if err != nil {
				return err
			}
			if err := tx.InsertOpening(ctx, w, openingID); err != nil {
				return err
			}
			zero, err := domain.Zero(balance.Currency())
			if err != nil {
				return err
			}
			return recordEvents(ctx, tx, openingID, w.ID(), "", "OPENING", "", "", "PROCESSED", "", balance, zero, balance, "CREDIT", w.Version(), now)
		}
		return nil
	})
	if err != nil {
		return WalletView{}, err
	}
	return View(w), nil
}
func replay(existing []Stored, hash [32]byte) (Result, error) {
	if len(existing) != 1 || len(existing[0].Hash) != 32 || string(existing[0].Hash) != string(hash[:]) {
		return Result{}, ErrConflict
	}
	result := existing[0].View.Result
	if result.Status != "PROCESSED" && result.Status != "REJECTED" && result.Status != "PENDING_REFERENCE" {
		return Result{}, ErrUnavailable
	}
	result.IdempotentReplay = true
	return result, nil
}
func (s *Service) Process(ctx context.Context, key string, o domain.Operation) (Result, error) {
	return s.process(ctx, key, o, nil)
}

func (s *Service) process(ctx context.Context, key string, o domain.Operation, message *MessageIdentity) (Result, error) {
	started := time.Now()
	var err error
	o.WalletID, err = UUID(o.WalletID)
	if err != nil {
		return Result{}, ErrInvalidInput
	}
	o.PlayerID, err = UUID(o.PlayerID)
	if err != nil {
		return Result{}, ErrInvalidInput
	}
	if key == "" || key != strings.TrimSpace(key) || len(key) > 256 {
		return Result{}, ErrInvalidInput
	}
	if err = o.Validate(); err != nil {
		return Result{}, errors.Join(ErrInvalidInput, err)
	}
	_, hash, err := CanonicalPayload(o)
	if err != nil {
		return Result{}, err
	}
	id, err := NewID()
	if err != nil {
		return Result{}, err
	}
	var result Result
	err = s.store.Within(ctx, func(tx Session) error {
		if message != nil {
			claimed, err := tx.ClaimMessage(ctx, "wager-transactions", message.ID, message.Hash[:])
			if err != nil {
				return err
			}
			_ = claimed // a confirmed duplicate continues to the persisted financial replay
		}
		return s.processInSession(ctx, tx, key, o, hash, id, &result, message)
	})
	if err != nil {
		if s.metrics != nil && errors.Is(err, ErrConcurrencyConflict) {
			s.metrics.ConcurrencyConflict()
		}
		return Result{}, err
	}
	if s.metrics != nil {
		s.metrics.ProcessResult(result.Status, result.IdempotentReplay, time.Since(started))
	}
	slog.Info("wager transaction completed", "correlationId", id, "messageId", messageID(message), "transactionId", result.TransactionID, "walletId", o.WalletID, "providerId", o.ProviderID, "status", result.Status, "idempotentReplay", result.IdempotentReplay)
	return result, nil
}

func (s *Service) processInSession(ctx context.Context, tx Session, key string, o domain.Operation, hash [32]byte, id string, result *Result, message *MessageIdentity) error {
	existing, err := tx.FindExisting(ctx, o.ProviderID, key, o.ExternalTransactionID)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		*result, err = replay(existing, hash)
		return err
	}
	w, err := tx.LockWallet(ctx, o.WalletID)
	if err != nil {
		return err
	}
	if w.PlayerID() != o.PlayerID || w.Balance().Currency() != o.Money.Currency() {
		return ErrInvalidInput
	}
	now := time.Now().UTC()
	if now.Before(w.UpdatedAt()) {
		now = w.UpdatedAt()
	}
	transaction, err := domain.NewTransaction(id, key, o, hash, now)
	if err != nil {
		return err
	}
	inserted, err := tx.Reserve(ctx, transaction)
	if err != nil {
		return err
	}
	if !inserted {
		existing, err = tx.FindExisting(ctx, o.ProviderID, key, o.ExternalTransactionID)
		if err != nil {
			return err
		}
		*result, err = replay(existing, hash)
		return err
	}
	d, err := inspectReference(ctx, tx, o)
	if err != nil {
		return err
	}
	if d.waitReason != "" {
		if err = transaction.AwaitReference(w.Balance(), now); err != nil {
			return err
		}
		if err = tx.MarkPending(ctx, transaction, 0, now.Add(ReferenceBaseBackoff), now.Add(ReferenceTTL)); err != nil {
			return err
		}
		*result = Result{TransactionID: id, Status: transaction.Status(), Balance: w.Balance()}
		return recordEvents(ctx, tx, id, o.WalletID, o.ProviderID, o.Kind, id, messageID(message), transaction.Status(), "", o.Money, w.Balance(), w.Balance(), "", w.Version(), now)
	}
	*result, err = finishMovement(ctx, tx, transaction, w, d, now, false, messageID(message))
	return err
}
func (s *Service) Wallet(ctx context.Context, id string) (WalletView, error) {
	id, err := UUID(id)
	if err != nil {
		return WalletView{}, err
	}
	w, err := s.store.Wallet(ctx, id)
	if err != nil {
		return WalletView{}, err
	}
	return View(w), nil
}
func (s *Service) Transaction(ctx context.Context, id string) (TransactionView, error) {
	id, err := UUID(id)
	if err != nil {
		return TransactionView{}, err
	}
	return s.store.Transaction(ctx, id)
}
func (s *Service) ProviderTransaction(ctx context.Context, provider, external string) (TransactionView, error) {
	if provider == "" || external == "" {
		return TransactionView{}, ErrInvalidInput
	}
	return s.store.ProviderTransaction(ctx, provider, external)
}
func (s *Service) Ledger(ctx context.Context, id, cursor string, limit int) (LedgerPage, error) {
	id, err := UUID(id)
	if err != nil || limit < 1 || limit > 100 {
		return LedgerPage{}, ErrInvalidInput
	}
	return s.store.Ledger(ctx, id, cursor, limit)
}
func (s *Service) Reconcile(ctx context.Context, id string) (Reconciliation, error) {
	id, err := UUID(id)
	if err != nil {
		return Reconciliation{}, err
	}
	result, err := s.store.Reconcile(ctx, id)
	if err != nil {
		return Reconciliation{}, err
	}
	if !result.Consistent {
		slog.Warn("wallet reconciliation divergence", "walletId", id, "checkedEntries", result.CheckedEntries)
		if s.metrics != nil {
			s.metrics.ReconciliationDivergence()
		}
	}
	return result, nil
}
