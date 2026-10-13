DO $$
DECLARE target_schema text := current_schema();
BEGIN
 EXECUTE format('REVOKE USAGE ON SCHEMA %I FROM wagering_runtime', target_schema);
 EXECUTE format('REVOKE SELECT,INSERT,UPDATE ON TABLE %I.wallet,%I.wager_transaction FROM wagering_runtime', target_schema,target_schema);
 EXECUTE format('REVOKE SELECT,INSERT ON TABLE %I.wallet_ledger_entry,%I.inbox_message FROM wagering_runtime', target_schema,target_schema);
 EXECUTE format('REVOKE SELECT,INSERT,UPDATE ON TABLE %I.outbox_event FROM wagering_runtime', target_schema);
END;
$$;
DROP ROLE IF EXISTS wagering_runtime;
