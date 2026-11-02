-- +goose Up
-- A read-only admin has platform grants like any other but may not change anything.
ALTER TABLE admins ADD COLUMN read_only INTEGER NOT NULL DEFAULT 0;
