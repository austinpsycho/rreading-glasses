CREATE TABLE IF NOT EXISTS "cache" (
  "key" TEXT NOT NULL PRIMARY KEY,
  "value" BYTEA NOT NULL,
  "expires" TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '7 day'
);
CREATE INDEX IF NOT EXISTS cache_expires_idx ON "cache" (expires);

-- Surrogate IDs for Audible ASINs.
--
-- The client parses every ForeignId as a signed 32-bit integer, so ASINs
-- (which are alphanumeric) can't be handed to it directly. IDs are allocated
-- from this sequence and persisted so they stay stable across reboots; a hash
-- would be smaller but risks collisions, and an ID collision silently
-- corrupts a library.
CREATE SEQUENCE IF NOT EXISTS asin_id_seq AS INTEGER START WITH 1000 MAXVALUE 2147483647 NO CYCLE;

CREATE TABLE IF NOT EXISTS "asin_id" (
  "kind"  TEXT NOT NULL,
  "asin"  TEXT NOT NULL,
  "id"    INTEGER NOT NULL UNIQUE DEFAULT nextval('asin_id_seq'),
  "label" TEXT,
  PRIMARY KEY ("kind", "asin")
);
CREATE INDEX IF NOT EXISTS asin_id_id_idx ON "asin_id" ("id");
