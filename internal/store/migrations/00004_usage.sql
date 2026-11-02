-- +goose Up
-- Hourly usage copied from new-api's /api/data endpoints. hour is the Unix time of the start of the
-- hour, as new-api stores it. quota is in new-api's raw units.
CREATE TABLE usage_model(
  platform_id INTEGER NOT NULL REFERENCES platforms(id) ON DELETE CASCADE,
  hour INTEGER NOT NULL, model TEXT NOT NULL,
  count INTEGER NOT NULL, quota INTEGER NOT NULL, tokens INTEGER NOT NULL,
  PRIMARY KEY(platform_id, hour, model)) WITHOUT ROWID;
CREATE TABLE usage_user(
  platform_id INTEGER NOT NULL REFERENCES platforms(id) ON DELETE CASCADE,
  hour INTEGER NOT NULL, username TEXT NOT NULL,
  count INTEGER NOT NULL, quota INTEGER NOT NULL, tokens INTEGER NOT NULL,
  PRIMARY KEY(platform_id, hour, username)) WITHOUT ROWID;
-- collected_to: everything before this time was fetched. quota_per_unit converts quota to currency.
CREATE TABLE usage_state(
  platform_id INTEGER PRIMARY KEY REFERENCES platforms(id) ON DELETE CASCADE,
  collected_to INTEGER NOT NULL, quota_per_unit REAL NOT NULL);
