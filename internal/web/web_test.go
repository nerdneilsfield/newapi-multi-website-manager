package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/nerdneilsfield/newapi-multi-website-manager/internal/manager"
	"github.com/nerdneilsfield/newapi-multi-website-manager/internal/store"
)

func newServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h, _ := bcrypt.GenerateFromPassword([]byte("right-password"), bcrypt.MinCost)
	if _, err := st.CreateAdmin("root", string(h), true, nil); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st, manager.New(t.Context(), st, zap.NewNop(), time.Millisecond)).Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func login(srv *httptest.Server, user, pass string) int {
	body := `{"username":"` + user + `","password":"` + pass + `"}`
	resp, err := http.Post(srv.URL+"/api/login", "application/json", strings.NewReader(body))
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func TestLoginLocksOutAfterRepeatedFailures(t *testing.T) {
	srv, _ := newServer(t)
	for i := 0; i < maxLoginFails; i++ {
		if c := login(srv, "root", "wrong"); c != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d", i, c)
		}
	}
	// Even the right password is refused while locked.
	if c := login(srv, "root", "right-password"); c != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", c)
	}
}

func TestPlatformRejectsURLWithCredentials(t *testing.T) {
	srv, _ := newServer(t)
	jar := loginJar(t, srv)
	req, _ := http.NewRequest("POST", srv.URL+"/api/platforms", strings.NewReader(`{"name":"x","base_url":"https://u:p@example.com","token":"t"}`))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range jar {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", resp.StatusCode)
	}
}

func loginJar(t *testing.T, srv *httptest.Server) []*http.Cookie {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/login", "application/json", strings.NewReader(`{"username":"root","password":"right-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.Cookies()
}

func call(t *testing.T, srv *httptest.Server, cookies []*http.Cookie, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestAuditIsScopedToTheAdmin(t *testing.T) {
	srv, _ := newServer(t)
	rootC := loginJar(t, srv)
	if c, b := call(t, srv, rootC, "POST", "/api/platforms", `{"name":"p1","base_url":"https://a.example.com","token":"secret-token"}`); c != 200 {
		t.Fatal(c, b)
	}
	if c, b := call(t, srv, rootC, "POST", "/api/admins", `{"username":"bob","password":"bob-password","platform_ids":[1]}`); c != 200 {
		t.Fatal(c, b)
	}
	resp, err := http.Post(srv.URL+"/api/login", "application/json", strings.NewReader(`{"username":"bob","password":"bob-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	bobC := resp.Cookies()

	_, rootLog := call(t, srv, rootC, "GET", "/api/audit", "")
	_, bobLog := call(t, srv, bobC, "GET", "/api/audit", "")
	for _, want := range []string{`"platform.create"`, `"admin.create"`, `"admin":"bob"`} {
		if !strings.Contains(rootLog, want) {
			t.Errorf("root log lacks %s: %s", want, rootLog)
		}
	}
	if strings.Contains(bobLog, `"admin":"root"`) || !strings.Contains(bobLog, `"login"`) {
		t.Errorf("bob must see only his own entries: %s", bobLog)
	}
	if strings.Contains(rootLog, "secret-token") {
		t.Error("the platform token must never reach the log")
	}

	// Deleting bob keeps his entries for root. An admin created later under the same name, and so
	// with the same reused id, must not inherit them.
	if c, _ := call(t, srv, rootC, "DELETE", "/api/admins/2", ""); c != 200 {
		t.Fatal(c)
	}
	call(t, srv, rootC, "POST", "/api/admins", `{"username":"bob","password":"bob2-password"}`)
	resp, _ = http.Post(srv.URL+"/api/login", "application/json", strings.NewReader(`{"username":"bob","password":"bob2-password"}`))
	_ = resp.Body.Close()
	_, newLog := call(t, srv, resp.Cookies(), "GET", "/api/audit", "")
	if strings.Contains(newLog, `"platform.create"`) || strings.Count(newLog, `"login"`) != 1 {
		t.Errorf("the new bob inherited the old bob's entries: %s", newLog)
	}
	_, rootLog = call(t, srv, rootC, "GET", "/api/audit", "")
	if strings.Count(rootLog, `"admin":"bob"`) != 2 {
		t.Errorf("root must still see the old bob's entry and the new one: %s", rootLog)
	}
}

func TestAuditPagesBackThroughHistory(t *testing.T) {
	srv, st := newServer(t)
	root, _ := st.AdminByName("root")
	for i := 0; i < auditLimit+50; i++ {
		_ = st.AddAudit(root, 0, "", "login", map[string]any{})
	}
	c := loginJar(t, srv) // adds one more entry
	var first, second []store.Audit
	_, b1 := call(t, srv, c, "GET", "/api/audit", "")
	_ = json.Unmarshal([]byte(b1), &first)
	_, b2 := call(t, srv, c, "GET", "/api/audit?before="+strconv.FormatInt(first[len(first)-1].ID, 10), "")
	_ = json.Unmarshal([]byte(b2), &second)
	if len(first) != auditLimit || len(second) != 51 || second[0].ID >= first[len(first)-1].ID {
		t.Fatalf("pages of %d and %d", len(first), len(second))
	}
}
