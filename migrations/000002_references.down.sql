DROP TRIGGER reversal_reference_check ON wager_transaction;
DROP FUNCTION validate_reversal_reference();
DROP INDEX one_successful_compensation;
DROP INDEX pending_reference_due;
ALTER TABLE wager_transaction DROP CONSTRAINT reference_pending_schedule;
ALTER TABLE wager_transaction DROP COLUMN reference_attempts, DROP COLUMN reference_next_attempt_at, DROP COLUMN reference_expires_at;
CREATE OR REPLACE FUNCTION validate_ledger_transaction() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE tx wager_transaction;
BEGIN
 SELECT * INTO STRICT tx FROM wager_transaction WHERE id=NEW.transaction_id FOR SHARE;
 IF tx.status<>'PROCESSED' OR tx.kind='LOSS' OR tx.amount_cents<>NEW.amount_cents
 OR tx.result_balance_cents<>NEW.balance_after_cents
 OR (tx.kind='BET' AND NEW.direction<>'DEBIT')
 OR (tx.kind IN ('OPENING','WIN','REFUND') AND NEW.direction<>'CREDIT') THEN
  RAISE EXCEPTION 'ledger does not match processed transaction' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;
