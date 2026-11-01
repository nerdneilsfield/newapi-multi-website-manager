// Package store persists admins, platforms, scheduled jobs and run history in SQLite.
package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

const schema = `
CREATE TABLE IF NOT EXISTS admins(
  id INTEGER PRIMARY KEY, username TEXT UNIQUE NOT NULL, pass_hash TEXT NOT NULL, is_root INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS platforms(
  id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, base_url TEXT NOT NULL, token TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS admin_platforms(
  admin_id INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
  platform_id INTEGER NOT NULL REFERENCES platforms(id) ON DELETE CASCADE,
  PRIMARY KEY(admin_id, platform_id));
CREATE TABLE IF NOT EXISTS snapshots(
  platform_id INTEGER PRIMARY KEY REFERENCES platforms(id) ON DELETE CASCADE, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS jobs(
  id INTEGER PRIMARY KEY, platform_id INTEGER NOT NULL REFERENCES platforms(id) ON DELETE CASCADE,
  name TEXT NOT NULL, kind TEXT NOT NULL, spec TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1, plan TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS runs(
  id INTEGER PRIMARY KEY, platform_id INTEGER NOT NULL REFERENCES platforms(id) ON DELETE CASCADE,
  job_id INTEGER, kind TEXT NOT NULL, title TEXT NOT NULL, by TEXT NOT NULL, status TEXT NOT NULL,
  total INTEGER NOT NULL DEFAULT 0, ok INTEGER NOT NULL DEFAULT 0, fail INTEGER NOT NULL DEFAULT 0,
  errors TEXT NOT NULL DEFAULT '', started_at INTEGER NOT NULL, finished_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS audit(
  id INTEGER PRIMARY KEY, at INTEGER NOT NULL, admin_id INTEGER NOT NULL, admin TEXT NOT NULL,
  platform_id INTEGER, platform TEXT NOT NULL DEFAULT '', action TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '{}');
CREATE INDEX IF NOT EXISTS audit_admin ON audit(admin_id, id);
`

// Store wraps the SQLite database.
type Store struct{ db *sql.DB }

// Open opens (creating with mode 0600 if needed) the database at path.
// Platform tokens are stored in plaintext, so the file mode is the only protection.
func Open(path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	if err = os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// ---- admins ----

// Admin is a manager login. Root admins manage admins and platforms and see every platform.
type Admin struct {
	ID          int64
	Username    string
	PassHash    string
	IsRoot      bool
	PlatformIDs []int64
}

func (s *Store) scanAdmin(row interface{ Scan(...any) error }) (Admin, error) {
	var a Admin
	if err := row.Scan(&a.ID, &a.Username, &a.PassHash, &a.IsRoot); err != nil {
		return a, err
	}
	rows, err := s.db.Query(`SELECT platform_id FROM admin_platforms WHERE admin_id=? ORDER BY platform_id`, a.ID)
	if err != nil {
		return a, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return a, err
		}
		a.PlatformIDs = append(a.PlatformIDs, id)
	}
	return a, rows.Err()
}

// AdminByName looks an admin up by username.
func (s *Store) AdminByName(name string) (Admin, error) {
	return s.scanAdmin(s.db.QueryRow(`SELECT id,username,pass_hash,is_root FROM admins WHERE username=?`, name))
}

// Admin looks an admin up by id.
func (s *Store) Admin(id int64) (Admin, error) {
	return s.scanAdmin(s.db.QueryRow(`SELECT id,username,pass_hash,is_root FROM admins WHERE id=?`, id))
}

// Admins lists all admins.
func (s *Store) Admins() ([]Admin, error) {
	rows, err := s.db.Query(`SELECT id FROM admins ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	var out []Admin
	for _, id := range ids {
		a, err := s.Admin(id)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// CountAdmins returns the number of admins.
func (s *Store) CountAdmins() (n int, err error) {
	err = s.db.QueryRow(`SELECT COUNT(*) FROM admins`).Scan(&n)
	return
}

func setGrants(tx *sql.Tx, adminID int64, platformIDs []int64) error {
	if _, err := tx.Exec(`DELETE FROM admin_platforms WHERE admin_id=?`, adminID); err != nil {
		return err
	}
	for _, p := range platformIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO admin_platforms(admin_id,platform_id) VALUES(?,?)`, adminID, p); err != nil {
			return err
		}
	}
	return nil
}

// CreateAdmin inserts an admin with its platform grants.
func (s *Store) CreateAdmin(username, hash string, root bool, platformIDs []int64) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`INSERT INTO admins(username,pass_hash,is_root) VALUES(?,?,?)`, username, hash, root)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if err := setGrants(tx, id, platformIDs); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// UpdateAdmin changes root flag, grants and (when hash != "") the password hash.
func (s *Store) UpdateAdmin(id int64, hash string, root bool, platformIDs []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE admins SET is_root=? WHERE id=?`, root, id); err != nil {
		return err
	}
	if hash != "" {
		if _, err := tx.Exec(`UPDATE admins SET pass_hash=? WHERE id=?`, hash, id); err != nil {
			return err
		}
	}
	if err := setGrants(tx, id, platformIDs); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteAdmin removes an admin.
func (s *Store) DeleteAdmin(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM admins WHERE id=?`, id); err != nil {
		return err
	}
	// SQLite reuses the largest id after a delete. Detach the old entries from it so an admin
	// created later with the same id cannot read them; root still sees them by name.
	if _, err := tx.Exec(`UPDATE audit SET admin_id=0 WHERE admin_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- platforms ----

// Platform is one managed new-api instance.
type Platform struct {
	ID      int64
	Name    string
	BaseURL string
	Token   string
}

// Platform returns one platform.
func (s *Store) Platform(id int64) (Platform, error) {
	var p Platform
	err := s.db.QueryRow(`SELECT id,name,base_url,token FROM platforms WHERE id=?`, id).Scan(&p.ID, &p.Name, &p.BaseURL, &p.Token)
	return p, err
}

// Platforms lists the platforms an admin may see (all of them for root).
func (s *Store) Platforms(a Admin) ([]Platform, error) {
	q := `SELECT id,name,base_url,token FROM platforms ORDER BY name`
	args := []any{}
	if !a.IsRoot {
		q = `SELECT p.id,p.name,p.base_url,p.token FROM platforms p JOIN admin_platforms g ON g.platform_id=p.id WHERE g.admin_id=? ORDER BY p.name`
		args = append(args, a.ID)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Platform
	for rows.Next() {
		var p Platform
		if err := rows.Scan(&p.ID, &p.Name, &p.BaseURL, &p.Token); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CreatePlatform inserts a platform.
func (s *Store) CreatePlatform(p Platform) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO platforms(name,base_url,token) VALUES(?,?,?)`, p.Name, p.BaseURL, p.Token)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdatePlatform updates a platform; an empty Token keeps the stored one.
func (s *Store) UpdatePlatform(p Platform) error {
	_, err := s.db.Exec(`UPDATE platforms SET name=?, base_url=?, token=CASE WHEN ?='' THEN token ELSE ? END WHERE id=?`,
		p.Name, p.BaseURL, p.Token, p.Token, p.ID)
	return err
}

// DeletePlatform removes a platform with its jobs, runs and snapshot.
func (s *Store) DeletePlatform(id int64) error {
	_, err := s.db.Exec(`DELETE FROM platforms WHERE id=?`, id)
	return err
}

// CanAccess reports whether the admin may operate on the platform.
func (s *Store) CanAccess(a Admin, platformID int64) bool {
	if a.IsRoot {
		return true
	}
	for _, id := range a.PlatformIDs {
		if id == platformID {
			return true
		}
	}
	return false
}

// ---- snapshots ----

// SaveSnapshot stores the latest statistics JSON for a platform.
func (s *Store) SaveSnapshot(platformID int64, data []byte) error {
	_, err := s.db.Exec(`INSERT INTO snapshots(platform_id,data) VALUES(?,?) ON CONFLICT(platform_id) DO UPDATE SET data=excluded.data`, platformID, string(data))
	return err
}

// Snapshot returns the latest statistics JSON, or sql.ErrNoRows.
func (s *Store) Snapshot(platformID int64) (data []byte, err error) {
	var str string
	err = s.db.QueryRow(`SELECT data FROM snapshots WHERE platform_id=?`, platformID).Scan(&str)
	return []byte(str), err
}

// ---- jobs ----

// QuotaPlan describes a balance adjustment: Mode is add, subtract or override;
// Unit is usd (converted with the platform's quota_per_unit) or quota (raw).
// Exactly one of Group and UserIDs selects the targets.
type QuotaPlan struct {
	Mode    string  `json:"mode"`
	Amount  float64 `json:"amount"`
	Unit    string  `json:"unit"`
	Group   string  `json:"group,omitempty"`
	UserIDs []int64 `json:"user_ids,omitempty"`
}

// Job is a scheduled task: kind "refresh" updates statistics, "quota" applies Plan.
type Job struct {
	ID         int64      `json:"id"`
	PlatformID int64      `json:"platform_id"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	Spec       string     `json:"spec"`
	Enabled    bool       `json:"enabled"`
	Plan       *QuotaPlan `json:"plan,omitempty"`
}

const jobCols = `id,platform_id,name,kind,spec,enabled,plan`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var plan string
	if err := row.Scan(&j.ID, &j.PlatformID, &j.Name, &j.Kind, &j.Spec, &j.Enabled, &plan); err != nil {
		return j, err
	}
	if plan != "" {
		j.Plan = new(QuotaPlan)
		if err := json.Unmarshal([]byte(plan), j.Plan); err != nil {
			return j, err
		}
	}
	return j, nil
}

func planJSON(p *QuotaPlan) string {
	if p == nil {
		return ""
	}
	b, _ := json.Marshal(p)
	return string(b)
}

// Job returns one job.
func (s *Store) Job(id int64) (Job, error) {
	return scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id))
}

func (s *Store) jobs(where string, args ...any) ([]Job, error) {
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM jobs `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Jobs lists a platform's jobs.
func (s *Store) Jobs(platformID int64) ([]Job, error) {
	return s.jobs(`WHERE platform_id=?`, platformID)
}

// EnabledJobs lists every enabled job on every platform.
func (s *Store) EnabledJobs() ([]Job, error) { return s.jobs(`WHERE enabled=1`) }

// CreateJob inserts a job.
func (s *Store) CreateJob(j Job) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO jobs(platform_id,name,kind,spec,enabled,plan) VALUES(?,?,?,?,?,?)`,
		j.PlatformID, j.Name, j.Kind, j.Spec, j.Enabled, planJSON(j.Plan))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateJob replaces a job's editable fields.
func (s *Store) UpdateJob(j Job) error {
	_, err := s.db.Exec(`UPDATE jobs SET name=?,kind=?,spec=?,enabled=?,plan=? WHERE id=?`,
		j.Name, j.Kind, j.Spec, j.Enabled, planJSON(j.Plan), j.ID)
	return err
}

// DeleteJob removes a job; its run history stays.
func (s *Store) DeleteJob(id int64) error {
	_, err := s.db.Exec(`DELETE FROM jobs WHERE id=?`, id)
	return err
}

// ---- runs ----

// Run is one execution of a refresh or quota operation.
type Run struct {
	ID         int64  `json:"id"`
	PlatformID int64  `json:"platform_id"`
	JobID      *int64 `json:"job_id"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	By         string `json:"by"`
	Status     string `json:"status"` // running, ok, partial, failed, interrupted, skipped
	Total      int    `json:"total"`
	OK         int    `json:"ok"`
	Fail       int    `json:"fail"`
	Errors     string `json:"errors"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`
}

// CreateRun records a new running run.
func (s *Store) CreateRun(platformID int64, jobID *int64, kind, title, by string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO runs(platform_id,job_id,kind,title,by,status,started_at) VALUES(?,?,?,?,?,'running',?)`,
		platformID, jobID, kind, title, by, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Progress records a running run's counters so a crash leaves them at the last known value.
func (s *Store) Progress(id int64, total, ok, fail int) error {
	_, err := s.db.Exec(`UPDATE runs SET total=?,ok=?,fail=? WHERE id=?`, total, ok, fail, id)
	return err
}

// FinishRun stores a run's outcome.
func (s *Store) FinishRun(id int64, status string, total, ok, fail int, errs string) error {
	_, err := s.db.Exec(`UPDATE runs SET status=?,total=?,ok=?,fail=?,errors=?,finished_at=? WHERE id=?`,
		status, total, ok, fail, errs, time.Now().Unix(), id)
	return err
}

// Run returns one run.
func (s *Store) Run(id int64) (Run, error) {
	var r Run
	err := s.db.QueryRow(`SELECT id,platform_id,job_id,kind,title,by,status,total,ok,fail,errors,started_at,finished_at FROM runs WHERE id=?`, id).
		Scan(&r.ID, &r.PlatformID, &r.JobID, &r.Kind, &r.Title, &r.By, &r.Status, &r.Total, &r.OK, &r.Fail, &r.Errors, &r.StartedAt, &r.FinishedAt)
	return r, err
}

// Runs lists a platform's most recent runs.
func (s *Store) Runs(platformID int64, limit int) ([]Run, error) {
	rows, err := s.db.Query(`SELECT id FROM runs WHERE platform_id=? ORDER BY id DESC LIMIT ?`, platformID, limit)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	var out []Run
	for _, id := range ids {
		r, err := s.Run(id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// InterruptRunning marks runs left "running" by a previous process as interrupted.
func (s *Store) InterruptRunning() error {
	_, err := s.db.Exec(`UPDATE runs SET status='interrupted',finished_at=? WHERE status='running'`, time.Now().Unix())
	return err
}

// ---- audit ----

// Audit is one operator action. It keeps the admin and platform names as text with no foreign
// keys, so the entry survives deleting either of them.
type Audit struct {
	ID         int64           `json:"id"`
	At         int64           `json:"at"`
	AdminID    int64           `json:"admin_id"`
	Admin      string          `json:"admin"`
	PlatformID *int64          `json:"platform_id"`
	Platform   string          `json:"platform"`
	Action     string          `json:"action"`
	Detail     json.RawMessage `json:"detail"`
}

// AddAudit records an action. platformID 0 means the action is not tied to a platform.
func (s *Store) AddAudit(by Admin, platformID int64, platform, action string, detail any) error {
	d, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	var pid any
	if platformID != 0 {
		pid = platformID
	}
	_, err = s.db.Exec(`INSERT INTO audit(at,admin_id,admin,platform_id,platform,action,detail) VALUES(?,?,?,?,?,?,?)`,
		time.Now().Unix(), by.ID, by.Username, pid, platform, action, string(d))
	return err
}

// Audits returns up to limit entries older than id before (0 means from the newest): all of them
// for root, only the admin's own otherwise.
func (s *Store) Audits(a Admin, before int64, limit int) ([]Audit, error) {
	q, args := `SELECT id,at,admin_id,admin,platform_id,platform,action,detail FROM audit WHERE (?=0 OR id<?)`, []any{before, before}
	if !a.IsRoot {
		q += ` AND admin_id=?`
		args = append(args, a.ID)
	}
	rows, err := s.db.Query(q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Audit{}
	for rows.Next() {
		var e Audit
		var d string
		if err := rows.Scan(&e.ID, &e.At, &e.AdminID, &e.Admin, &e.PlatformID, &e.Platform, &e.Action, &d); err != nil {
			return nil, err
		}
		e.Detail = json.RawMessage(d)
		out = append(out, e)
	}
	return out, rows.Err()
}
