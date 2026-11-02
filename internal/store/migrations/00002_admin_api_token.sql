-- +goose Up
-- One API token per admin, stored as is (like the platform tokens). An empty value means no token.
ALTER TABLE admins ADD COLUMN api_token TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX admins_api_token ON admins(api_token) WHERE api_token <> '';
DROP TABLE IF EXISTS admin_tokens;
