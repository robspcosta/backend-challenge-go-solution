-- Local development credential only. Replace the role credential through deployment tooling outside Compose.
DO $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='wagering_runtime') THEN
  CREATE ROLE wagering_runtime LOGIN PASSWORD 'local-only-runtime';
 END IF;
END;
$$;
DO $$
DECLARE target_schema text := current_schema();
BEGIN
 EXECUTE format('GRANT USAGE ON SCHEMA %I TO wagering_runtime', target_schema);
 EXECUTE format('GRANT SELECT,INSERT,UPDATE ON TABLE %I.wallet,%I.wager_transaction TO wagering_runtime', target_schema,target_schema);
 EXECUTE format('GRANT SELECT,INSERT ON TABLE %I.wallet_ledger_entry,%I.inbox_message TO wagering_runtime', target_schema,target_schema);
 EXECUTE format('GRANT SELECT,INSERT,UPDATE ON TABLE %I.outbox_event TO wagering_runtime', target_schema);
END;
$$;
