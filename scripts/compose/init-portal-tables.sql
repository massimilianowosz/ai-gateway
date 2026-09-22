-- Tabelle di proprietà del portale, non del gateway.
--
-- Le migration del gateway creano le proprie tabelle (gw_*, tenant_settings,
-- key_route_settings) ma non queste due: in produzione le crea il portale e il
-- gateway le legge soltanto. Su un volume vuoto il gateway parte comunque, poi
-- fallisce alla prima chiave con:
--
--   ERROR: relation "api_keys_local" does not exist (SQLSTATE 42P01)
--
-- Questo file gira una sola volta, all'inizializzazione del volume Postgres.
-- Le colonne seguono i tag gorm di store.APIKey e store.Team.

CREATE TABLE IF NOT EXISTS api_keys_local (
  id             text PRIMARY KEY,
  litellm_key_id text UNIQUE NOT NULL,
  key_prefix     text,
  key_name       text,
  team_id        text,
  budget         double precision DEFAULT 0,
  spend          double precision DEFAULT 0,
  rate_limit     integer DEFAULT 0,
  models         text,
  active         boolean DEFAULT true,
  expires_at     timestamptz,
  created_at     timestamptz,
  updated_at     timestamptz
);
CREATE INDEX IF NOT EXISTS idx_api_keys_local_team_id ON api_keys_local (team_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_local_active  ON api_keys_local (active);

CREATE TABLE IF NOT EXISTS tenants (
  id                  text PRIMARY KEY,
  litellm_team_id     text UNIQUE NOT NULL,
  name                text NOT NULL,
  budget              double precision DEFAULT 0,
  spend               double precision DEFAULT 0,
  allowed_models      text,
  granted_models      text,
  subscription_tier   text DEFAULT 'free',
  subscription_status text DEFAULT 'active',
  created_at          timestamptz,
  updated_at          timestamptz
);
