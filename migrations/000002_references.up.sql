ALTER TABLE wager_transaction
 ADD COLUMN reference_attempts INTEGER NOT NULL DEFAULT 0 CHECK (reference_attempts >= 0),
 ADD COLUMN reference_next_attempt_at TIMESTAMPTZ,
 ADD COLUMN reference_expires_at TIMESTAMPTZ;
ALTER TABLE wager_transaction ADD CONSTRAINT reference_pending_schedule CHECK (
 (status='PENDING_REFERENCE' AND reference_external_transaction_id IS NOT NULL AND reference_next_attempt_at IS NOT NULL AND reference_expires_at IS NOT NULL)
 OR (status<>'PENDING_REFERENCE' AND reference_next_attempt_at IS NULL AND reference_expires_at IS NULL)
);
CREATE INDEX pending_reference_due ON wager_transaction(reference_next_attempt_at,id) WHERE status='PENDING_REFERENCE';
-- A bet can be refunded or rolled back, never credited twice. For a REFUND,
-- the only valid reversal is ROLLBACK; the same index also prevents its replay.
CREATE UNIQUE INDEX one_successful_compensation ON wager_transaction(reference_transaction_id)
 WHERE status='PROCESSED' AND kind IN ('REFUND','ROLLBACK') AND reference_transaction_id IS NOT NULL;
CREATE FUNCTION validate_reversal_reference() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE original wager_transaction;
BEGIN
 IF NEW.status='PROCESSED' AND NEW.kind IN ('REFUND','ROLLBACK') THEN
  IF NEW.reference_transaction_id IS NULL THEN
   RAISE EXCEPTION 'processed reversal needs a reference' USING ERRCODE='23514';
  END IF;
  SELECT * INTO STRICT original FROM wager_transaction WHERE id=NEW.reference_transaction_id;
  IF original.status<>'PROCESSED' OR original.provider_id<>NEW.provider_id OR original.wallet_id<>NEW.wallet_id
     OR original.player_id<>NEW.player_id OR original.currency<>NEW.currency OR original.round_id<>NEW.round_id
     OR original.amount_cents<>NEW.amount_cents OR original.external_transaction_id<>NEW.reference_external_transaction_id
     OR (NEW.kind='REFUND' AND original.kind<>'BET')
     OR (NEW.kind='ROLLBACK' AND original.kind NOT IN ('BET','WIN','REFUND')) THEN
   RAISE EXCEPTION 'invalid reversal reference' USING ERRCODE='23514';
  END IF;
 END IF;
 RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER reversal_reference_check AFTER INSERT OR UPDATE ON wager_transaction
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION validate_reversal_reference();
CREATE OR REPLACE FUNCTION validate_ledger_transaction() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE tx wager_transaction; referenced_kind TEXT; expected_direction TEXT;
BEGIN
 SELECT * INTO STRICT tx FROM wager_transaction WHERE id=NEW.transaction_id FOR SHARE;
 IF tx.kind='ROLLBACK' THEN
  SELECT kind INTO referenced_kind FROM wager_transaction WHERE id=tx.reference_transaction_id;
  expected_direction:=CASE WHEN referenced_kind='BET' THEN 'CREDIT' WHEN referenced_kind IN ('WIN','REFUND') THEN 'DEBIT' ELSE NULL END;
 ELSE
  expected_direction:=CASE WHEN tx.kind='BET' THEN 'DEBIT' WHEN tx.kind IN ('OPENING','WIN','REFUND') THEN 'CREDIT' ELSE NULL END;
 END IF;
 IF tx.status<>'PROCESSED' OR tx.kind='LOSS' OR tx.amount_cents<>NEW.amount_cents
 OR tx.result_balance_cents<>NEW.balance_after_cents OR expected_direction IS NULL OR NEW.direction<>expected_direction THEN
  RAISE EXCEPTION 'ledger does not match processed transaction' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;
