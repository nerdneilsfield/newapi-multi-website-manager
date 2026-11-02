// Package store persists admins, platforms, scheduled jobs and run history in SQLite.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"io/fs"
	"os"
	"time"

	"github.com/georgysavva/scany/v2/sqlscan"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

//go:embed migrations/*.sql
var migrations embed.FS

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
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

// migrate brings the schema up to date. To change the schema, add the next numbered file under migrations/.
func migrate(db *sql.DB) error {
	dir, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, dir)
	if err != nil {
		return err
	}
	_, err = p.Up(context.Background())
	return err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// ---- admins ----

// Admin is a manager login. Root admins manage admins and platforms and see every platform.
type Admin struct {
	ID          int64   `db:"id"`
	Username    string  `db:"username"`
	PassHash    string  `db:"pass_hash"`
	IsRoot      bool    `db:"is_root"`
	APIToken    string  `db:"api_token"` // "" when the admin has none
	PlatformIDs []int64 `db:"-"`
}

const adminCols = `id,username,pass_hash,is_root,api_token`

func (s *Store) grants(a *Admin) error {
	return sqlscan.Select(context.Background(), s.db, &a.PlatformIDs, `SELECT platform_id FROM admin_platforms WHERE admin_id=? ORDER BY platform_id`, a.ID)
}

// AdminByName looks an admin up by username.
func (s *Store) AdminByName(name string) (a Admin, err error) {
	if err = sqlscan.Get(context.Background(), s.db, &a, `SELECT `+adminCols+` FROM admins WHERE username=?`, name); err != nil {
		return a, err
	}
	return a, s.grants(&a)
}

// Admin looks an admin up by id.
func (s *Store) Admin(id int64) (a Admin, err error) {
	if err = sqlscan.Get(context.Background(), s.db, &a, `SELECT `+adminCols+` FROM admins WHERE id=?`, id); err != nil {
		return a, err
	}
	return a, s.grants(&a)
}

// Admins lists all admins.
func (s *Store) Admins() ([]Admin, error) {
	var out []Admin
	if err := sqlscan.Select(context.Background(), s.db, &out, `SELECT `+adminCols+` FROM admins ORDER BY id`); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.grants(&out[i]); err != nil {
			return nil, err
		}
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

// UpdateAdmin changes root flag, grants and (when hash != "") the password hash, which also revokes the admin's API token.
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
		// A new password is the answer to a leaked credential, so the API token goes with the sessions.
		if _, err := tx.Exec(`UPDATE admins SET pass_hash=?, api_token='' WHERE id=?`, hash, id); err != nil {
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
	ID      int64  `db:"id"`
	Name    string `db:"name"`
	BaseURL string `db:"base_url"`
	Token   string `db:"token"`
}

// Platform returns one platform.
func (s *Store) Platform(id int64) (p Platform, err error) {
	err = sqlscan.Get(context.Background(), s.db, &p, `SELECT id,name,base_url,token FROM platforms WHERE id=?`, id)
	return p, err
}

// Platforms lists the platforms an admin may see (all of them for root).
func (s *Store) Platforms(a Admin) (out []Platform, err error) {
	q, args := `SELECT id,name,base_url,token FROM platforms ORDER BY name`, []any{}
	if !a.IsRoot {
		q = `SELECT p.id,p.name,p.base_url,p.token FROM platforms p JOIN admin_platforms g ON g.platform_id=p.id WHERE g.admin_id=? ORDER BY p.name`
		args = append(args, a.ID)
	}
	err = sqlscan.Select(context.Background(), s.db, &out, q, args...)
	return out, err
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
	ID         int64      `json:"id" db:"id"`
	PlatformID int64      `json:"platform_id" db:"platform_id"`
	Name       string     `json:"name" db:"name"`
	Kind       string     `json:"kind" db:"kind"`
	Spec       string     `json:"spec" db:"spec"`
	Enabled    bool       `json:"enabled" db:"enabled"`
	Plan       *QuotaPlan `json:"plan,omitempty" db:"-"`
}

// jobRow is a Job as stored: the plan is a JSON column.
type jobRow struct {
	Job
	PlanJSON string `db:"plan"`
}

func (r jobRow) job() (Job, error) {
	j := r.Job
	if r.PlanJSON != "" {
		j.Plan = new(QuotaPlan)
		if err := json.Unmarshal([]byte(r.PlanJSON), j.Plan); err != nil {
			return j, err
		}
	}
	return j, nil
}

const jobCols = `id,platform_id,name,kind,spec,enabled,plan`

func planJSON(p *QuotaPlan) string {
	if p == nil {
		return ""
	}
	b, _ := json.Marshal(p)
	return string(b)
}

// Job returns one job.
func (s *Store) Job(id int64) (Job, error) {
	var r jobRow
	if err := sqlscan.Get(context.Background(), s.db, &r, `SELECT `+jobCols+` FROM jobs WHERE id=?`, id); err != nil {
		return Job{}, err
	}
	return r.job()
}

func (s *Store) jobs(where string, args ...any) ([]Job, error) {
	var rs []jobRow
	if err := sqlscan.Select(context.Background(), s.db, &rs, `SELECT `+jobCols+` FROM jobs `+where+` ORDER BY id`, args...); err != nil {
		return nil, err
	}
	var out []Job
	for _, r := range rs {
		j, err := r.job()
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
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
	ID         int64  `json:"id" db:"id"`
	PlatformID int64  `json:"platform_id" db:"platform_id"`
	JobID      *int64 `json:"job_id" db:"job_id"`
	Kind       string `json:"kind" db:"kind"`
	Title      string `json:"title" db:"title"`
	By         string `json:"by" db:"by"`
	Status     string `json:"status" db:"status"` // running, ok, partial, failed, interrupted, skipped
	Total      int    `json:"total" db:"total"`
	OK         int    `json:"ok" db:"ok"`
	Fail       int    `json:"fail" db:"fail"`
	Errors     string `json:"errors" db:"errors"`
	StartedAt  int64  `json:"started_at" db:"started_at"`
	FinishedAt int64  `json:"finished_at" db:"finished_at"`
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

const runCols = `id,platform_id,job_id,kind,title,by,status,total,ok,fail,errors,started_at,finished_at`

// Run returns one run.
func (s *Store) Run(id int64) (r Run, err error) {
	err = sqlscan.Get(context.Background(), s.db, &r, `SELECT `+runCols+` FROM runs WHERE id=?`, id)
	return r, err
}

// Runs lists a platform's most recent runs.
func (s *Store) Runs(platformID int64, limit int) (out []Run, err error) {
	err = sqlscan.Select(context.Background(), s.db, &out, `SELECT `+runCols+` FROM runs WHERE platform_id=? ORDER BY id DESC LIMIT ?`, platformID, limit)
	return out, err
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
	ID         int64           `json:"id" db:"id"`
	At         int64           `json:"at" db:"at"`
	AdminID    int64           `json:"admin_id" db:"admin_id"`
	Admin      string          `json:"admin" db:"admin"`
	PlatformID *int64          `json:"platform_id" db:"platform_id"`
	Platform   string          `json:"platform" db:"platform"`
	Action     string          `json:"action" db:"action"`
	Detail     json.RawMessage `json:"detail" db:"-"`
}

// auditRow is an Audit as stored: the detail is a JSON text column.
type auditRow struct {
	Audit
	DetailJSON string `db:"detail"`
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
	var rs []auditRow
	if err := sqlscan.Select(context.Background(), s.db, &rs, q+` ORDER BY id DESC LIMIT ?`, append(args, limit)...); err != nil {
		return nil, err
	}
	out := make([]Audit, len(rs))
	for i, r := range rs {
		out[i] = r.Audit
		out[i].Detail = json.RawMessage(r.DetailJSON)
	}
	return out, nil
}

// ---- API token ----

// SetAPIToken sets the admin's API token, replacing any earlier one; "" removes it.
// It returns sql.ErrNoRows when there is no such admin.
func (s *Store) SetAPIToken(adminID int64, token string) error {
	res, err := s.db.Exec(`UPDATE admins SET api_token=? WHERE id=?`, token, adminID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AdminByToken returns the admin that owns this API token.
func (s *Store) AdminByToken(token string) (Admin, error) {
	if token == "" {
		return Admin{}, sql.ErrNoRows
	}
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM admins WHERE api_token=?`, token).Scan(&id); err != nil {
		return Admin{}, err
	}
	return s.Admin(id)
}
