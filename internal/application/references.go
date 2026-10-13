package application

import (
	"context"
	"errors"
	"example.com/wagering/internal/domain"
	"log/slog"
	"time"
)

const (
	ReferenceMaxAttempts = 8
	ReferenceTTL         = 30 * time.Minute
	ReferenceBaseBackoff = 250 * time.Millisecond
)

type referenceDecision struct{ referenceID, waitReason, failure, direction string }

func inspectReference(ctx context.Context, tx Session, o domain.Operation) (referenceDecision, error) {
	d := referenceDecision{}
	if o.ReferenceExternalTransactionID == "" {
		switch o.Kind {
		case "BET":
			d.direction = "DEBIT"
		case "WIN":
			d.direction = "CREDIT"
		}
		return d, nil
	}
	original, err := tx.Reference(ctx, o.ProviderID, o.ReferenceExternalTransactionID)
	if errors.Is(err, ErrNotFound) {
		d.waitReason = "REFERENCE_NOT_FOUND"
		return d, nil
	}
	if err != nil {
		return d, err
	}
	d.referenceID = original.View.TransactionID
	r := original.View.Operation
	if original.View.Status == "PENDING" || original.View.Status == "PENDING_REFERENCE" {
		d.waitReason = "REFERENCE_NOT_PROCESSED"
		return d, nil
	}
	if original.View.Status != "PROCESSED" {
		d.failure = "REFERENCE_NOT_PROCESSED"
		return d, nil
	}
	if r.PlayerID != o.PlayerID || r.WalletID != o.WalletID || r.RoundID != o.RoundID || r.Money.Currency() != o.Money.Currency() {
		d.failure = "INVALID_REFERENCE"
		return d, nil
	}
	switch o.Kind {
	case "WIN":
		if r.Kind != "BET" {
			d.failure = "INVALID_REFERENCE"
			return d, nil
		}
		d.direction = "CREDIT"
	case "REFUND", "ROLLBACK":
		if r.Money.Cents() != o.Money.Cents() {
			d.failure = "INVALID_REFERENCE"
			return d, nil
		}
		if (o.Kind == "REFUND" && r.Kind != "BET") || (o.Kind == "ROLLBACK" && r.Kind != "BET" && r.Kind != "WIN" && r.Kind != "REFUND") {
			d.failure = "INVALID_REFERENCE"
			return d, nil
		}
		taken, err := tx.CompensationExists(ctx, d.referenceID)
		if err != nil {
			return d, err
		}
		if taken {
			d.failure = "ALREADY_REVERSED"
			return d, nil
		}
		if o.Kind == "REFUND" || r.Kind == "BET" {
			d.direction = "CREDIT"
		} else {
			d.direction = "DEBIT"
		}
	}
	return d, nil
}
func finishMovement(ctx context.Context, tx Session, t *domain.Transaction, w *domain.Wallet, d referenceDecision, now time.Time, pending bool, causationID string) (Result, error) {
	before := w.Balance()
	previousVersion := w.Version()
	failure := d.failure
	if failure == "" {
		var err error
		switch d.direction {
		case "DEBIT":
			err = w.Debit(t.Operation().Money, now)
		case "CREDIT":
			err = w.Credit(t.Operation().Money, now)
		}
		if err != nil {
			switch {
			case errors.Is(err, domain.ErrInsufficientFunds):
				if t.Operation().Kind == "BET" {
					failure = "INSUFFICIENT_FUNDS"
				} else {
					failure = "REVERSAL_INSUFFICIENT_FUNDS"
				}
			case errors.Is(err, domain.ErrOverflow):
				failure = "NUMERIC_LIMIT"
			default:
				return Result{}, err
			}
		}
	}
	if err := t.Complete(w.Balance(), failure, now); err != nil {
		return Result{}, err
	}
	var err error
	if pending {
		err = tx.ResolveReference(ctx, t, d.referenceID)
	} else {
		err = tx.Finish(ctx, t, d.referenceID)
	}
	if err != nil {
		return Result{}, err
	}
	if failure == "" && d.direction != "" {
		if err = tx.SaveWallet(ctx, w, previousVersion); err != nil {
			return Result{}, err
		}
		if err = tx.AppendLedger(ctx, t, before, d.direction); err != nil {
			return Result{}, err
		}
	}
	if err := recordEvents(ctx, tx, t.ID(), w.ID(), t.Operation().ProviderID, t.Operation().Kind, t.ID(), causationID, t.Status(), failure, t.Operation().Money, before, w.Balance(), d.direction, w.Version(), now); err != nil {
		return Result{}, err
	}
	return Result{TransactionID: t.ID(), Status: t.Status(), Balance: w.Balance(), FailureCode: failure}, nil
}
func referenceDelay(attempt int) time.Duration {
	if attempt < 1 {
		return ReferenceBaseBackoff
	}
	if attempt > ReferenceMaxAttempts {
		attempt = ReferenceMaxAttempts
	}
	return ReferenceBaseBackoff * time.Duration(1<<attempt)
}

// ResolveDue is safe to run from several processes; SKIP LOCKED assigns a row to one worker.
func (s *Service) ResolveDue(ctx context.Context, id string) error {
	started := time.Now()
	var completed *Result
	err := s.store.Within(ctx, func(tx Session) error {
		t, attempts, expires, err := tx.LockPending(ctx, id)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		o := t.Operation()
		w, err := tx.LockWallet(ctx, o.WalletID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if now.Before(w.UpdatedAt()) {
			now = w.UpdatedAt()
		}
		if now.Before(t.UpdatedAt()) {
			now = t.UpdatedAt()
		}
		d, err := inspectReference(ctx, tx, o)
		if err != nil {
			return err
		}
		if attempts >= ReferenceMaxAttempts || !now.Before(expires) {
			if d.waitReason == "REFERENCE_NOT_PROCESSED" {
				d.failure = "REFERENCE_NOT_PROCESSED"
			} else {
				d.failure = "REFERENCE_NOT_FOUND"
			}
			d.waitReason = ""
			d.referenceID = ""
			d.direction = ""
		}
		if d.waitReason != "" {
			attempts++
			if attempts >= ReferenceMaxAttempts {
				d.failure = d.waitReason
				d.waitReason = ""
				d.referenceID = ""
			} else {
				next := now.Add(referenceDelay(attempts))
				if next.After(expires) {
					next = expires
				}
				return tx.Reschedule(ctx, t.ID(), attempts, next)
			}
		}
		result, finishErr := finishMovement(ctx, tx, t, w, d, now, true, "")
		err = finishErr
		if err == nil {
			completed = &result
		}
		return err
	})
	if err == nil && completed != nil {
		if s.metrics != nil {
			s.metrics.ProcessResult(completed.Status, false, time.Since(started))
		}
		slog.Info("pending wager transaction completed", "transactionId", completed.TransactionID, "status", completed.Status)
	}
	return err
}
func (s *Service) ProcessDue(ctx context.Context, limit int) (int, error) {
	ids, err := s.store.DueReferences(ctx, time.Now().UTC(), limit)
	if err != nil {
		return 0, err
	}
	completed := 0
	for _, id := range ids {
		if err = s.ResolveDue(ctx, id); err != nil {
			return completed, err
		}
		completed++
	}
	return completed, nil
}
