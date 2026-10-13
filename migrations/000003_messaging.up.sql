CREATE TABLE inbox_message (
 consumer_name TEXT NOT NULL CHECK (length(trim(consumer_name))>0),
 message_id TEXT NOT NULL CHECK (length(trim(message_id))>0),
 payload_hash BYTEA NOT NULL CHECK (octet_length(payload_hash)=32),
 received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 completed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY (consumer_name,message_id)
);
CREATE TABLE outbox_event (
 event_id UUID PRIMARY KEY,
 aggregate_id UUID NOT NULL REFERENCES wallet(id),
 event_type TEXT NOT NULL CHECK (event_type IN ('WagerTransactionProcessed','WagerTransactionRejected','WagerTransactionPendingReference','WalletBalanceChanged')),
 payload JSONB NOT NULL CHECK (jsonb_typeof(payload)='object'),
 occurred_at TIMESTAMPTZ NOT NULL,
 attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts>=0),
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 lease_owner UUID,
 lease_until TIMESTAMPTZ,
 published_at TIMESTAMPTZ,
 last_error TEXT,
 CHECK ((lease_owner IS NULL)=(lease_until IS NULL)),
 CHECK (published_at IS NULL OR lease_owner IS NULL)
);
CREATE INDEX outbox_due ON outbox_event(next_attempt_at,event_id) WHERE published_at IS NULL;
CREATE FUNCTION protect_outbox_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.event_id<>OLD.event_id OR NEW.aggregate_id<>OLD.aggregate_id OR NEW.event_type<>OLD.event_type
 OR NEW.payload<>OLD.payload OR NEW.occurred_at<>OLD.occurred_at THEN
  RAISE EXCEPTION 'outbox event snapshot is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER outbox_snapshot_guard BEFORE UPDATE ON outbox_event FOR EACH ROW EXECUTE FUNCTION protect_outbox_snapshot();
CREATE FUNCTION protect_outbox_deletion() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'outbox is append-only' USING ERRCODE='23514'; END;
$$;
CREATE TRIGGER outbox_no_delete BEFORE DELETE ON outbox_event FOR EACH ROW EXECUTE FUNCTION protect_outbox_deletion();
CREATE TRIGGER outbox_no_truncate BEFORE TRUNCATE ON outbox_event FOR EACH STATEMENT EXECUTE FUNCTION protect_outbox_deletion();
CREATE FUNCTION protect_inbox() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'inbox is append-only' USING ERRCODE='23514'; END;
$$;
CREATE TRIGGER inbox_guard BEFORE UPDATE OR DELETE ON inbox_message FOR EACH ROW EXECUTE FUNCTION protect_inbox();
CREATE TRIGGER inbox_no_truncate BEFORE TRUNCATE ON inbox_message FOR EACH STATEMENT EXECUTE FUNCTION protect_inbox();
