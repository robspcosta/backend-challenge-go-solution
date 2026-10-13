CREATE TABLE wallet (
 id UUID PRIMARY KEY,
 player_id UUID NOT NULL,
 currency TEXT NOT NULL CHECK (currency IN ('BRL','USD','EUR')),
 balance_cents BIGINT NOT NULL CHECK (balance_cents >= 0),
 version BIGINT NOT NULL DEFAULT 1 CHECK (version >= 1),
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now() CHECK (updated_at >= created_at),
 UNIQUE (player_id,currency),
 UNIQUE (id,player_id,currency)
);
CREATE TABLE wager_transaction (
 id UUID PRIMARY KEY,
 wallet_id UUID NOT NULL,
 player_id UUID NOT NULL,
 currency TEXT NOT NULL,
 kind TEXT NOT NULL CHECK (kind IN ('OPENING','BET','WIN','LOSS','REFUND','ROLLBACK')),
 amount_cents BIGINT NOT NULL,
 provider_id TEXT,
 external_transaction_id TEXT,
 idempotency_key TEXT,
 payload_hash BYTEA,
 round_id TEXT,
 game_id TEXT,
 reference_external_transaction_id TEXT,
 reference_transaction_id UUID REFERENCES wager_transaction(id),
 status TEXT NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','PENDING_REFERENCE','PROCESSED','REJECTED','FAILED')),
 failure_code TEXT,
 result_balance_cents BIGINT CHECK (result_balance_cents >= 0),
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now() CHECK (updated_at >= created_at),
 FOREIGN KEY (wallet_id,player_id,currency) REFERENCES wallet(id,player_id,currency),
 UNIQUE (id,wallet_id,currency),
 UNIQUE (provider_id,external_transaction_id),
 UNIQUE (provider_id,idempotency_key),
 CHECK ((kind='LOSS' AND amount_cents=0) OR (kind<>'LOSS' AND amount_cents>0)),
 CHECK (
  (kind='OPENING' AND status='PROCESSED' AND provider_id IS NULL AND external_transaction_id IS NULL
   AND idempotency_key IS NULL AND payload_hash IS NULL AND round_id IS NULL AND game_id IS NULL
   AND reference_external_transaction_id IS NULL AND reference_transaction_id IS NULL)
  OR
  (kind<>'OPENING' AND length(trim(provider_id))>0 AND provider_id IS NOT NULL
   AND length(trim(external_transaction_id))>0 AND external_transaction_id IS NOT NULL
   AND length(trim(idempotency_key))>0 AND idempotency_key IS NOT NULL
   AND octet_length(payload_hash)=32 AND payload_hash IS NOT NULL
   AND length(trim(round_id))>0 AND round_id IS NOT NULL
   AND length(trim(game_id))>0 AND game_id IS NOT NULL)
 ),
 CHECK (kind NOT IN ('REFUND','ROLLBACK') OR (reference_external_transaction_id IS NOT NULL AND length(trim(reference_external_transaction_id))>0)),
 CHECK ((status IN ('REJECTED','FAILED') AND failure_code IS NOT NULL AND length(trim(failure_code))>0)
    OR (status NOT IN ('REJECTED','FAILED') AND failure_code IS NULL)),
 CHECK (status<>'PROCESSED' OR result_balance_cents IS NOT NULL)
);
CREATE UNIQUE INDEX one_opening_per_wallet ON wager_transaction(wallet_id) WHERE kind='OPENING';
CREATE UNIQUE INDEX one_reversal_per_kind ON wager_transaction(reference_transaction_id,kind)
 WHERE status='PROCESSED' AND kind IN ('REFUND','ROLLBACK');
CREATE TABLE wallet_ledger_entry (
 id UUID PRIMARY KEY,
 wallet_id UUID NOT NULL,
 transaction_id UUID NOT NULL,
 currency TEXT NOT NULL,
 direction TEXT NOT NULL CHECK (direction IN ('DEBIT','CREDIT')),
 amount_cents BIGINT NOT NULL CHECK (amount_cents>0),
 balance_before_cents BIGINT NOT NULL CHECK (balance_before_cents>=0),
 balance_after_cents BIGINT NOT NULL CHECK (balance_after_cents>=0),
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 UNIQUE (wallet_id,transaction_id),
 FOREIGN KEY (transaction_id,wallet_id,currency) REFERENCES wager_transaction(id,wallet_id,currency),
 -- NUMERIC intermediates prevent overflow while checking the BIGINT equation.
 CHECK (balance_after_cents::NUMERIC = balance_before_cents::NUMERIC +
  CASE direction WHEN 'CREDIT' THEN amount_cents::NUMERIC ELSE -amount_cents::NUMERIC END)
);
CREATE INDEX ledger_pagination ON wallet_ledger_entry(wallet_id,created_at,id);
CREATE FUNCTION protect_ledger() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'ledger is append-only' USING ERRCODE='23514'; END;
$$;
CREATE TRIGGER ledger_no_update_delete BEFORE UPDATE OR DELETE ON wallet_ledger_entry
 FOR EACH ROW EXECUTE FUNCTION protect_ledger();
CREATE TRIGGER ledger_no_truncate BEFORE TRUNCATE ON wallet_ledger_entry
 FOR EACH STATEMENT EXECUTE FUNCTION protect_ledger();
CREATE FUNCTION validate_ledger_transaction() RETURNS trigger LANGUAGE plpgsql AS $$
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
CREATE TRIGGER ledger_validate BEFORE INSERT ON wallet_ledger_entry
 FOR EACH ROW EXECUTE FUNCTION validate_ledger_transaction();

CREATE FUNCTION check_wallet_financial_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE target UUID; stored wallet; reconstructed NUMERIC; movements BIGINT;
BEGIN
 IF TG_TABLE_NAME='wallet' THEN target=NEW.id; ELSE target=NEW.wallet_id; END IF;
 SELECT * INTO STRICT stored FROM wallet WHERE id=target FOR UPDATE;
 SELECT COALESCE(SUM(CASE l.direction WHEN 'CREDIT' THEN l.amount_cents::NUMERIC ELSE -l.amount_cents::NUMERIC END),0),
 COUNT(*) FILTER (WHERE t.kind<>'OPENING') INTO reconstructed,movements
 FROM wallet_ledger_entry l JOIN wager_transaction t ON t.id=l.transaction_id WHERE l.wallet_id=target;
 IF stored.balance_cents::NUMERIC<>reconstructed OR stored.version::NUMERIC<>1+movements::NUMERIC THEN
  RAISE EXCEPTION 'wallet snapshot differs from ledger' USING ERRCODE='23514';
 END IF;
 RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER wallet_financial_snapshot AFTER INSERT OR UPDATE ON wallet
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_wallet_financial_snapshot();
CREATE CONSTRAINT TRIGGER ledger_financial_snapshot AFTER INSERT ON wallet_ledger_entry
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_wallet_financial_snapshot();
CREATE FUNCTION check_transaction_ledger() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE current_tx wager_transaction; entries BIGINT;
BEGIN
 SELECT * INTO STRICT current_tx FROM wager_transaction WHERE id=NEW.id;
 SELECT COUNT(*) INTO entries FROM wallet_ledger_entry WHERE transaction_id=NEW.id;
 IF (current_tx.status='PROCESSED' AND current_tx.kind<>'LOSS' AND entries<>1)
 OR ((current_tx.status<>'PROCESSED' OR current_tx.kind='LOSS') AND entries<>0) THEN
  RAISE EXCEPTION 'transaction ledger cardinality invalid' USING ERRCODE='23514';
 END IF;
 RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER transaction_ledger AFTER INSERT OR UPDATE ON wager_transaction
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_transaction_ledger();
CREATE FUNCTION protect_terminal_transaction() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.status IN ('PROCESSED','REJECTED','FAILED') THEN
  RAISE EXCEPTION 'terminal transaction is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER transaction_terminal BEFORE UPDATE OR DELETE ON wager_transaction
 FOR EACH ROW EXECUTE FUNCTION protect_terminal_transaction();
