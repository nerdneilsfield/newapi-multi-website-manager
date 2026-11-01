package manager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/nerdneilsfield/newapi-multi-website-manager/internal/store"
)

// fakeNewAPI mimics the new-api admin endpoints the manager uses (response
// shapes taken from QuantumNous/new-api controller code).
type fakeNewAPI struct {
	mu    sync.Mutex
	quota map[int64]int64
	calls []map[string]any
}

func (f *fakeNewAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := func(data any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "", "data": data})
	}
	if r.URL.Path != "/api/status" && r.Header.Get("Authorization") != "Bearer nap_test" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/api/status":
		ok(map[string]any{"quota_per_unit": 500000})
	case "/api/group/":
		ok([]string{"default", "vip"})
	case "/api/user/search":
		users := []map[string]any{}
		for _, u := range []struct {
			id    int64
			name  string
			group string
		}{{1, "a", "default"}, {2, "b", "vip"}, {3, "c", "vip"}} {
			if g := r.URL.Query().Get("group"); g != "" && g != u.group {
				continue
			}
			users = append(users, map[string]any{"id": u.id, "username": u.name, "group": u.group, "quota": f.quota[u.id], "used_quota": 10, "request_count": 2})
		}
		ok(map[string]any{"items": users, "total": len(users), "page": 1, "page_size": 100})
	case "/api/user/manage":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.calls = append(f.calls, b)
		id, v := int64(b["id"].(float64)), int64(b["value"].(float64))
		switch b["mode"] {
		case "add":
			f.quota[id] += v
		case "subtract":
			f.quota[id] -= v
		case "override":
			f.quota[id] = v
		}
		ok(nil)
	case "/api/user/1":
		ok(map[string]any{"id": 1, "username": "a", "group": "default", "quota": f.quota[1]})
	default:
		http.NotFound(w, r)
	}
}

func setup(t *testing.T) (*Manager, *store.Store, *fakeNewAPI, int64) {
	t.Helper()
	fake := &fakeNewAPI{quota: map[int64]int64{1: 100, 2: 1000, 3: 5}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	pid, err := st.CreatePlatform(store.Platform{Name: "p", BaseURL: srv.URL, Token: "nap_test"})
	if err != nil {
		t.Fatal(err)
	}
	return New(context.Background(), st, zap.NewNop(), time.Millisecond), st, fake, pid
}

func waitRun(t *testing.T, st *store.Store, id int64) store.Run {
	t.Helper()
	for i := 0; i < 200; i++ {
		r, _ := st.Run(id)
		if r.Status != "running" {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run did not finish")
	return store.Run{}
}

func TestRefreshAggregatesByGroup(t *testing.T) {
	m, st, _, pid := setup(t)
	id, err := m.Refresh(pid, nil, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if r := waitRun(t, st, id); r.Status != "ok" {
		t.Fatalf("status %s: %s", r.Status, r.Errors)
	}
	data, _ := st.Snapshot(pid)
	var s Stats
	_ = json.Unmarshal(data, &s)
	if s.Users != 3 || s.Quota != 1105 || len(s.Groups) != 2 || s.Groups[1].Name != "vip" || s.Groups[1].Quota != 1005 {
		t.Fatalf("unexpected stats: %+v", s)
	}
}

func TestQuotaGroupUsdAndGuards(t *testing.T) {
	m, st, fake, pid := setup(t)

	// 0.001 USD = 500 quota, converted with the platform's quota_per_unit.
	id, err := m.ApplyQuota(pid, nil, "", "test", store.QuotaPlan{Mode: "add", Amount: 0.001, Unit: "usd", Group: "vip"})
	if err != nil {
		t.Fatal(err)
	}
	if r := waitRun(t, st, id); r.Status != "ok" || r.OK != 2 {
		t.Fatalf("run: %+v", r)
	}
	if fake.quota[1] != 100 || fake.quota[2] != 1500 || fake.quota[3] != 505 {
		t.Fatalf("quota: %v", fake.quota)
	}

	// subtract larger than the balance is refused locally; user 2 succeeds.
	id, _ = m.ApplyQuota(pid, nil, "", "test", store.QuotaPlan{Mode: "subtract", Amount: 600, Unit: "quota", Group: "vip"})
	r := waitRun(t, st, id)
	if r.Status != "partial" || r.OK != 1 || r.Fail != 1 || fake.quota[2] != 900 || fake.quota[3] != 505 {
		t.Fatalf("run: %+v quota %v", r, fake.quota)
	}

	// override always calls the API (it is idempotent), even when the snapshot already equals the target.
	n := len(fake.calls)
	id, _ = m.ApplyQuota(pid, nil, "", "test", store.QuotaPlan{Mode: "override", Amount: 100, Unit: "quota", UserIDs: []int64{1}})
	if r := waitRun(t, st, id); r.Status != "ok" || len(fake.calls) != n+1 || fake.quota[1] != 100 {
		t.Fatalf("run: %+v calls %d->%d quota %v", r, n, len(fake.calls), fake.quota)
	}
}

func TestQuotaBoundsAndProgress(t *testing.T) {
	m, st, fake, pid := setup(t)

	// An amount beyond the wallet ceiling is refused before any request is made.
	id, _ := m.ApplyQuota(pid, nil, "", "test", store.QuotaPlan{Mode: "add", Amount: 1e30, Unit: "quota", UserIDs: []int64{1}})
	if r := waitRun(t, st, id); r.Status != "failed" || len(fake.calls) != 0 {
		t.Fatalf("run: %+v calls %d", r, len(fake.calls))
	}

	// A user whose balance is already near the ceiling is skipped, not sent to the server.
	fake.mu.Lock()
	fake.quota[2] = maxWalletQuota - 10
	fake.mu.Unlock()
	id, _ = m.ApplyQuota(pid, nil, "", "test", store.QuotaPlan{Mode: "add", Amount: 100, Unit: "quota", Group: "vip"})
	r := waitRun(t, st, id)
	if r.Status != "partial" || r.OK != 1 || r.Fail != 1 || len(fake.calls) != 1 {
		t.Fatalf("run: %+v calls %d", r, len(fake.calls))
	}
}

func TestRepeatedUserIDIsAdjustedOnce(t *testing.T) {
	m, st, fake, pid := setup(t)
	id, _ := m.ApplyQuota(pid, nil, "", "test", store.QuotaPlan{Mode: "subtract", Amount: 60, Unit: "quota", UserIDs: []int64{1, 1}})
	if r := waitRun(t, st, id); r.Status != "ok" || r.Total != 1 || fake.quota[1] != 40 || len(fake.calls) != 1 {
		t.Fatalf("run: %+v quota %v calls %d", r, fake.quota, len(fake.calls))
	}
}

func TestOneRunPerPlatform(t *testing.T) {
	m, st, _, pid := setup(t)
	m.Interval = 50 * time.Millisecond // clients are created lazily, so this applies to the run below
	id, err := m.Refresh(pid, nil, "", "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Refresh(pid, nil, "", "b"); err != ErrBusy {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	waitRun(t, st, id)
}

func TestValidatePlan(t *testing.T) {
	bad := []store.QuotaPlan{
		{Mode: "add", Amount: 0, Unit: "usd", Group: "g"},
		{Mode: "nope", Amount: 1, Unit: "usd", Group: "g"},
		{Mode: "add", Amount: 1, Unit: "eur", Group: "g"},
		{Mode: "add", Amount: 1, Unit: "usd"},
		{Mode: "add", Amount: 1, Unit: "usd", Group: "g", UserIDs: []int64{1}},
	}
	for i, p := range bad {
		if ValidatePlan(p) == nil {
			t.Errorf("case %d should be invalid", i)
		}
	}
	if ValidatePlan(store.QuotaPlan{Mode: "override", Amount: 0, Unit: "quota", Group: "g"}) != nil {
		t.Error("override 0 should be valid")
	}
}
