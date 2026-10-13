\set ON_ERROR_STOP on
SELECT pg_advisory_lock(716001);
SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version=4) AS revert_v4 \gset
BEGIN;
\if :revert_v4
\ir 000004_runtime_role.down.sql
DELETE FROM schema_migrations WHERE version=4;
\endif
COMMIT;
SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version=3) AS revert_v3 \gset
BEGIN;
\if :revert_v3
\ir 000003_messaging.down.sql
DELETE FROM schema_migrations WHERE version=3;
\endif
COMMIT;
SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version=2) AS revert_v2 \gset
BEGIN;
\if :revert_v2
\ir 000002_references.down.sql
DELETE FROM schema_migrations WHERE version=2;
\endif
COMMIT;
SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version=1) AS revert_v1 \gset
BEGIN;
\if :revert_v1
\ir 000001_financial_schema.down.sql
DELETE FROM schema_migrations WHERE version=1;
\endif
COMMIT;
SELECT pg_advisory_unlock(716001);
