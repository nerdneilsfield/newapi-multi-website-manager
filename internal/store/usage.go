package store

import (
	"context"

	"github.com/georgysavva/scany/v2/sqlscan"
)

// UsageHour is one model's usage in one bucket (an hour, or a local day) of a platform.
// T is the Unix time the bucket starts.
type UsageHour struct {
	T      int64  `json:"t" db:"t"`
	Model  string `json:"model" db:"model"`
	Count  int64  `json:"count" db:"count"`
	Quota  int64  `json:"quota" db:"quota"`
	Tokens int64  `json:"tokens" db:"tokens"`
}

// UserHour is one user's usage in one bucket of a platform.
type UserHour struct {
	T        int64  `json:"t" db:"t"`
	Username string `json:"username" db:"username"`
	Count    int64  `json:"count" db:"count"`
	Quota    int64  `json:"quota" db:"quota"`
	Tokens   int64  `json:"tokens" db:"tokens"`
}

// UsageState says how far a platform's usage has been collected.
type UsageState struct {
	CollectedTo  int64   `json:"collected_to" db:"collected_to"`
	QuotaPerUnit float64 `json:"quota_per_unit" db:"quota_per_unit"`
}

// SaveUsage upserts the rows (new-api keeps adding to an hour until it is flushed, so a refetched
// hour replaces the old one) and moves the platform's watermark.
func (s *Store) SaveUsage(platformID int64, st UsageState, models []UsageHour, users []UserHour) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, m := range models {
		if _, err = tx.Exec(`INSERT INTO usage_model(platform_id,hour,model,count,quota,tokens) VALUES(?,?,?,?,?,?)
			ON CONFLICT(platform_id,hour,model) DO UPDATE SET count=excluded.count, quota=excluded.quota, tokens=excluded.tokens`,
			platformID, m.T, m.Model, m.Count, m.Quota, m.Tokens); err != nil {
			return err
		}
	}
	for _, u := range users {
		if _, err = tx.Exec(`INSERT INTO usage_user(platform_id,hour,username,count,quota,tokens) VALUES(?,?,?,?,?,?)
			ON CONFLICT(platform_id,hour,username) DO UPDATE SET count=excluded.count, quota=excluded.quota, tokens=excluded.tokens`,
			platformID, u.T, u.Username, u.Count, u.Quota, u.Tokens); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO usage_state(platform_id,collected_to,quota_per_unit) VALUES(?,?,?)
		ON CONFLICT(platform_id) DO UPDATE SET collected_to=excluded.collected_to, quota_per_unit=excluded.quota_per_unit`,
		platformID, st.CollectedTo, st.QuotaPerUnit); err != nil {
		return err
	}
	return tx.Commit()
}

// UsageState returns the watermark, or sql.ErrNoRows when nothing was collected yet.
func (s *Store) UsageState(platformID int64) (st UsageState, err error) {
	err = sqlscan.Get(context.Background(), s.db, &st, `SELECT collected_to,quota_per_unit FROM usage_state WHERE platform_id=?`, platformID)
	return st, err
}

// PruneUsage deletes usage older than the given hour.
func (s *Store) PruneUsage(platformID, before int64) error {
	for _, t := range []string{"usage_model", "usage_user"} {
		if _, err := s.db.Exec(`DELETE FROM `+t+` WHERE platform_id=? AND hour<?`, platformID, before); err != nil {
			return err
		}
	}
	return nil
}

// bucket is the SQL expression for the start of a row's bucket: the hour itself, or the start of
// its local day when tz (minutes east of UTC) is applied.
func bucket(day bool, tz int) (expr string, args []any) {
	if !day {
		return "hour", nil
	}
	off := int64(tz) * 60
	return "((hour+?)/86400)*86400-?", []any{off, off}
}

// UsageModels returns per-model usage in [from, to), by hour or summed into local days.
func (s *Store) UsageModels(platformID, from, to int64, day bool, tz int) ([]UsageHour, error) {
	b, args := bucket(day, tz)
	out := []UsageHour{}
	err := sqlscan.Select(context.Background(), s.db, &out, `SELECT `+b+` AS t, model, SUM(count) AS count, SUM(quota) AS quota, SUM(tokens) AS tokens
		FROM usage_model WHERE platform_id=? AND hour>=? AND hour<? GROUP BY t, model ORDER BY t, model`, append(args, platformID, from, to)...)
	return out, err
}

// UsageUsers returns per-user usage in [from, to), by hour or summed into local days.
func (s *Store) UsageUsers(platformID, from, to int64, day bool, tz int) ([]UserHour, error) {
	b, args := bucket(day, tz)
	out := []UserHour{}
	err := sqlscan.Select(context.Background(), s.db, &out, `SELECT `+b+` AS t, username, SUM(count) AS count, SUM(quota) AS quota, SUM(tokens) AS tokens
		FROM usage_user WHERE platform_id=? AND hour>=? AND hour<? GROUP BY t, username ORDER BY t, username`, append(args, platformID, from, to)...)
	return out, err
}
