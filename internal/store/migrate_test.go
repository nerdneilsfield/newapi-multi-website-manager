package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// A database made before napm had migrations has the tables but no goose version table.
// Opening it must keep its data and add what is missing.
func TestOpenAdoptsADatabaseFromBeforeMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`
CREATE TABLE admins(id INTEGER PRIMARY KEY, username TEXT UNIQUE NOT NULL, pass_hash TEXT NOT NULL, is_root INTEGER NOT NULL DEFAULT 0);
INSERT INTO admins(username, pass_hash, is_root) VALUES('root', 'h', 1);`); err != nil {
		t.Fatal(err)
	}
	_ = old.Close()

	for range 2 { // the second open must be a no-op
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		a, err := s.AdminByName("root")
		if err != nil || !a.IsRoot {
			t.Fatalf("existing admin lost: %+v, %v", a, err)
		}
		for _, table := range []string{"platforms", "admin_platforms", "snapshots", "jobs", "runs", "audit"} {
			if _, err := s.db.Exec(`SELECT 1 FROM ` + table + ` LIMIT 1`); err != nil {
				t.Errorf("table %s missing: %v", table, err)
			}
		}
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE version_id IN (1,2)`).Scan(&n); err != nil || n != 2 {
			t.Errorf("migrations recorded %d times, err=%v", n, err)
		}
		if a.APIToken != "" {
			t.Errorf("a new column must start empty: %q", a.APIToken)
		}
		_ = s.Close()
	}
}

func TestEmptyAuditEncodesAsAnArrayAndJobPlansRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	root := Admin{ID: 1, Username: "r", IsRoot: true}
	if a, aerr := s.Audits(root, 0, 10); aerr != nil || a == nil {
		t.Errorf("Audits on an empty table = %#v, %v; want a non-nil slice so JSON is []", a, aerr)
	}
	pid, err := s.CreatePlatform(Platform{Name: "p", BaseURL: "https://x.example", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	plan := &QuotaPlan{Mode: "add", Amount: 1.5, Unit: "usd", UserIDs: []int64{3, 4}}
	id, err := s.CreateJob(Job{PlatformID: pid, Name: "n", Kind: "quota", Spec: "@every 1h", Enabled: true, Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateJob(Job{PlatformID: pid, Name: "r", Kind: "refresh", Spec: "@every 1h"}); err != nil {
		t.Fatal(err)
	}
	j, err := s.Job(id)
	if err != nil || j.Plan == nil || j.Plan.Amount != 1.5 || len(j.Plan.UserIDs) != 2 || !j.Enabled {
		t.Fatalf("job = %+v, %v", j, err)
	}
	js, err := s.Jobs(pid)
	if err != nil || len(js) != 2 || js[1].Plan != nil {
		t.Fatalf("jobs = %+v, %v", js, err)
	}
	if _, err := s.Job(999); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("missing job: %v, want sql.ErrNoRows", err)
	}
}

func TestAdminWithoutATokenIsNotFoundByTheEmptyToken(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err = s.CreateAdmin("bob", "h", false, false, nil); err != nil {
		t.Fatal(err)
	}
	if a, aerr := s.AdminByToken(""); !errors.Is(aerr, sql.ErrNoRows) {
		t.Errorf("empty token matched %+v, %v", a, aerr)
	}
	if err = s.SetAPIToken(999, "napm_x"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("setting a token for a missing admin: %v", err)
	}
	if err = s.SetAPIToken(1, "napm_x"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateAdmin("carol", "h", false, false, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.SetAPIToken(2, "napm_x"); err == nil {
		t.Error("two admins must not share a token")
	}
}
