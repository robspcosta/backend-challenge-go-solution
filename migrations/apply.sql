\set ON_ERROR_STOP on
SELECT pg_advisory_lock(716001);
CREATE TABLE IF NOT EXISTS schema_migrations (version BIGINT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now());
SELECT NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version=1) AS apply_v1 \gset
BEGIN;
\if :apply_v1
\ir 000001_financial_schema.up.sql
INSERT INTO schema_migrations(version) VALUES (1);
\endif
COMMIT;
SELECT NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version=2) AS apply_v2 \gset
BEGIN;
\if :apply_v2
\ir 000002_references.up.sql
INSERT INTO schema_migrations(version) VALUES (2);
\endif
COMMIT;
SELECT NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version=3) AS apply_v3 \gset
BEGIN;
\if :apply_v3
\ir 000003_messaging.up.sql
INSERT INTO schema_migrations(version) VALUES (3);
\endif
COMMIT;
SELECT NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version=4) AS apply_v4 \gset
BEGIN;
\if :apply_v4
\ir 000004_runtime_role.up.sql
INSERT INTO schema_migrations(version) VALUES (4);
\endif
COMMIT;
SELECT pg_advisory_unlock(716001);
