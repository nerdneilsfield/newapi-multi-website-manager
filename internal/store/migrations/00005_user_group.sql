-- +goose Up
-- Each user's group as of the last statistics refresh. Usage rows carry only a username, so the usage
-- page uses this to total usage by group; a user who changed group is counted in the current one.
CREATE TABLE user_group(
  platform_id INTEGER NOT NULL REFERENCES platforms(id) ON DELETE CASCADE,
  username TEXT NOT NULL, grp TEXT NOT NULL,
  PRIMARY KEY(platform_id, username)) WITHOUT ROWID;
