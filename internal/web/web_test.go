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

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	if _, err := st.CreateAdmin("root", string(h), true, false, nil); err != nil {
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

// bearer sends a request with only an Authorization header, like an MCP client.
func bearer(t *testing.T, srv *httptest.Server, tok, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// newToken makes the signed-in admin's own token and returns it.
func newToken(t *testing.T, srv *httptest.Server, cookies []*http.Cookie, adminID int) string {
	t.Helper()
	c, b := call(t, srv, cookies, "POST", "/api/admins/"+strconv.Itoa(adminID)+"/token", "")
	var r struct{ Token string }
	if c != 200 || json.Unmarshal([]byte(b), &r) != nil || !strings.HasPrefix(r.Token, "napm_") {
		t.Fatal(c, b)
	}
	return r.Token
}

// mcpSession connects the official MCP client to /mcp with the given bearer token.
func mcpSession(t *testing.T, srv *httptest.Server, tok string) *mcp.ClientSession {
	t.Helper()
	hc := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+tok)
		return http.DefaultTransport.RoundTrip(r)
	})}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).
		Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// toolText runs a tool and returns whether it failed and its text.
func toolText(t *testing.T, srv *httptest.Server, tok, name, args string) (bool, string) {
	t.Helper()
	var a map[string]any
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		t.Fatal(err)
	}
	res, err := mcpSession(t, srv, tok).CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: a})
	if err != nil {
		return true, err.Error() // the SDK reports bad arguments as a protocol error
	}
	return res.IsError, res.Content[0].(*mcp.TextContent).Text
}

func TestTokenAuthenticatesTheRESTAPI(t *testing.T) {
	srv, _ := newServer(t)
	rootC := loginJar(t, srv)
	tok := newToken(t, srv, rootC, 1)

	if c, b := bearer(t, srv, tok, "GET", "/api/me", ""); c != 200 || !strings.Contains(b, `"root":true`) {
		t.Fatal(c, b)
	}
	// A token is not a cookie: no Content-Type or Origin is needed to write.
	if c, b := bearer(t, srv, tok, "POST", "/api/platforms", `{"name":"p1","base_url":"https://a.example.com","token":"x"}`); c != 200 {
		t.Fatal(c, b)
	}
	if c, _ := bearer(t, srv, tok+"0", "GET", "/api/me", ""); c != 401 {
		t.Fatalf("wrong token: %d", c)
	}
	// A bad token must not fall back to a valid cookie.
	if c, _ := call(t, srv, rootC, "GET", "/api/me", ""); c != 200 {
		t.Fatal(c)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	req.AddCookie(rootC[0])
	resp, _ := http.DefaultClient.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("bad token fell back to the cookie: %d", resp.StatusCode)
	}

	// A token cannot read, replace or delete tokens.
	for _, r := range [][3]string{{"GET", "/api/admins/1/token", ""}, {"POST", "/api/admins/1/token", ""}, {"DELETE", "/api/admins/1/token", ""}} {
		if c, _ := bearer(t, srv, tok, r[0], r[1], r[2]); c != 403 {
			t.Errorf("%s %s with a token: %d, want 403", r[0], r[1], c)
		}
	}
	if _, raw := call(t, srv, rootC, "GET", "/api/admins", ""); strings.Contains(raw, tok) {
		t.Error("the admin list must say whether a token exists, not show it")
	}

	// Revoking it stops it at once.
	if c, _ := call(t, srv, rootC, "DELETE", "/api/admins/1/token", ""); c != 200 {
		t.Fatal(c)
	}
	if c, _ := bearer(t, srv, tok, "GET", "/api/me", ""); c != 401 {
		t.Fatalf("revoked token: %d", c)
	}
}

func TestTokenOnlyReachesItsOwnersPlatforms(t *testing.T) {
	srv, _ := newServer(t)
	rootC := loginJar(t, srv)
	call(t, srv, rootC, "POST", "/api/platforms", `{"name":"p1","base_url":"https://a.example.com","token":"x"}`)
	call(t, srv, rootC, "POST", "/api/platforms", `{"name":"p2","base_url":"https://b.example.com","token":"y"}`)
	call(t, srv, rootC, "POST", "/api/admins", `{"username":"bob","password":"bob-password","platform_ids":[1]}`)
	resp, _ := http.Post(srv.URL+"/api/login", "application/json", strings.NewReader(`{"username":"bob","password":"bob-password"}`))
	_ = resp.Body.Close()
	bobTok := newToken(t, srv, resp.Cookies(), 2)

	if _, text := toolText(t, srv, bobTok, "list_platforms", `{}`); !strings.Contains(text, `"p1"`) || strings.Contains(text, `"p2"`) {
		t.Errorf("list_platforms: %s", text)
	}
	if failed, text := toolText(t, srv, bobTok, "get_stats", `{"platform_id":2}`); !failed || !strings.Contains(text, "无权访问") {
		t.Errorf("platform 2 must be refused: %v %s", failed, text)
	}
	if failed, _ := toolText(t, srv, bobTok, "list_runs", `{"platform_id":1}`); failed {
		t.Error("platform 1 must be allowed")
	}
	if failed, text := toolText(t, srv, bobTok, "get_stats", `{}`); !failed || !strings.Contains(text, "platform_id") {
		t.Errorf("a missing argument must be reported: %v %s", failed, text)
	}

	// Taking the grant away applies to the token at once.
	call(t, srv, rootC, "PUT", "/api/admins/2", `{"root":false,"platform_ids":[]}`)
	if failed, _ := toolText(t, srv, bobTok, "list_runs", `{"platform_id":1}`); !failed {
		t.Error("a revoked grant must apply to the token at once")
	}
	// Tool calls are audited under the token's owner, like the same REST calls.
	_, text := toolText(t, srv, bobTok, "list_audit", `{}`)
	if strings.Contains(text, `"admin":"root"`) {
		t.Errorf("bob sees root's entries: %s", text)
	}
}

func TestMCPEndpoint(t *testing.T) {
	srv, _ := newServer(t)
	rootC := loginJar(t, srv)
	tok := newToken(t, srv, rootC, 1)

	// No token, a bad token, or only a cookie, gets nothing.
	for name, c := range map[string][]*http.Cookie{"cookie": rootC, "anonymous": nil} {
		if code, _ := call(t, srv, c, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); code != 401 {
			t.Errorf("%s on /mcp: %d, want 401", name, code)
		}
	}
	if c, _ := bearer(t, srv, tok+"0", "POST", "/mcp", `{}`); c != 401 {
		t.Errorf("bad token on /mcp: %d, want 401", c)
	}

	cs := mcpSession(t, srv, tok)
	if cs.InitializeResult().ServerInfo.Name != "napm" {
		t.Errorf("server info: %+v", cs.InitializeResult().ServerInfo)
	}
	list, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, x := range list.Tools {
		names[x.Name] = true
	}
	for _, want := range []string{"list_platforms", "get_stats", "refresh_stats", "list_users", "adjust_quota", "list_runs", "list_jobs", "create_job", "update_job", "delete_job", "list_audit", "get_usage", "collect_usage"} {
		if !names[want] {
			t.Errorf("tool %s missing", want)
		}
	}
	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "no_such_tool"}); err == nil {
		t.Error("an unknown tool must be an error")
	}
}

func TestMCPToolsDriveJobsAndAreAudited(t *testing.T) {
	srv, _ := newServer(t)
	rootC := loginJar(t, srv)
	call(t, srv, rootC, "POST", "/api/platforms", `{"name":"p1","base_url":"https://a.example.com","token":"x"}`)
	tok := newToken(t, srv, rootC, 1)

	failed, text := toolText(t, srv, tok, "create_job", `{"platform_id":1,"name":"nightly","kind":"quota","spec":"0 3 * * *","enabled":true,"plan":{"mode":"override","amount":5,"unit":"usd","group":"trial"}}`)
	if failed {
		t.Fatal(text)
	}
	if _, text = toolText(t, srv, tok, "list_jobs", `{"platform_id":1}`); !strings.Contains(text, `"nightly"`) {
		t.Errorf("list_jobs: %s", text)
	}
	// REST validation applies unchanged.
	if failed, text = toolText(t, srv, tok, "create_job", `{"platform_id":1,"name":"bad","kind":"refresh","spec":"nonsense","enabled":true}`); !failed || !strings.Contains(text, "定时表达式无效") {
		t.Errorf("a bad spec must be refused: %v %s", failed, text)
	}
	if failed, _ = toolText(t, srv, tok, "delete_job", `{"platform_id":1,"job_id":1}`); failed {
		t.Error("delete_job failed")
	}
	_, text = toolText(t, srv, tok, "list_audit", `{}`)
	for _, want := range []string{`"job.create"`, `"job.delete"`} {
		if !strings.Contains(text, want) {
			t.Errorf("audit lacks %s: %s", want, text)
		}
	}
}

func TestPasswordChangeRevokesTokens(t *testing.T) {
	srv, _ := newServer(t)
	rootC := loginJar(t, srv)
	call(t, srv, rootC, "POST", "/api/admins", `{"username":"bob","password":"bob-password"}`)
	resp, _ := http.Post(srv.URL+"/api/login", "application/json", strings.NewReader(`{"username":"bob","password":"bob-password"}`))
	_ = resp.Body.Close()
	tok := newToken(t, srv, resp.Cookies(), 2)
	if c, _ := bearer(t, srv, tok, "GET", "/api/me", ""); c != 200 {
		t.Fatal(c)
	}
	call(t, srv, rootC, "PUT", "/api/admins/2", `{"password":"new-password-1"}`)
	if c, _ := bearer(t, srv, tok, "GET", "/api/me", ""); c != 401 {
		t.Errorf("token survived a password change: %d", c)
	}
	// Deleting the admin removes the token with the row.
	tok = newToken(t, srv, rootC, 2)
	call(t, srv, rootC, "DELETE", "/api/admins/2", "")
	if c, _ := bearer(t, srv, tok, "GET", "/api/me", ""); c != 401 {
		t.Errorf("token of a deleted admin: %d, want 401", c)
	}
}

func TestOneTokenPerAdminManagedByTheAdminOrRoot(t *testing.T) {
	srv, _ := newServer(t)
	rootC := loginJar(t, srv)
	call(t, srv, rootC, "POST", "/api/admins", `{"username":"bob","password":"bob-password"}`)
	call(t, srv, rootC, "POST", "/api/admins", `{"username":"carol","password":"carol-password"}`)
	bobC := loginAs(t, srv, "bob", "bob-password")
	read := func(c []*http.Cookie, id int) (int, string) {
		code, b := call(t, srv, c, "GET", "/api/admins/"+strconv.Itoa(id)+"/token", "")
		var r struct{ Token string }
		_ = json.Unmarshal([]byte(b), &r)
		return code, r.Token
	}

	// A new admin has none. The list says so.
	if _, tok := read(bobC, 2); tok != "" {
		t.Fatalf("a new admin has a token: %q", tok)
	}
	if _, raw := call(t, srv, rootC, "GET", "/api/admins", ""); !strings.Contains(raw, `"has_token":false`) {
		t.Errorf("list: %s", raw)
	}

	// Bob makes his own; he and root both read the same value.
	first := newToken(t, srv, bobC, 2)
	if _, got := read(bobC, 2); got != first {
		t.Errorf("bob reads %q, want %q", got, first)
	}
	if _, got := read(rootC, 2); got != first {
		t.Errorf("root reads %q, want %q", got, first)
	}
	if _, raw := call(t, srv, rootC, "GET", "/api/admins", ""); !strings.Contains(raw, `"has_token":true`) {
		t.Errorf("list: %s", raw)
	}

	// Making another replaces it: there is only ever one.
	second := newToken(t, srv, bobC, 2)
	if second == first {
		t.Fatal("regenerating returned the same token")
	}
	if c, _ := bearer(t, srv, first, "GET", "/api/me", ""); c != 401 {
		t.Errorf("replaced token still works: %d", c)
	}
	if c, _ := bearer(t, srv, second, "GET", "/api/me", ""); c != 200 {
		t.Errorf("new token: %d", c)
	}

	// Root makes one for carol, who never had a token, and carol can use it.
	carolTok := newToken(t, srv, rootC, 3)
	if c, b := bearer(t, srv, carolTok, "GET", "/api/me", ""); c != 200 || !strings.Contains(b, "carol") {
		t.Errorf("carol's token: %d %s", c, b)
	}

	// Bob may not touch carol's token or root's, in any way.
	for _, r := range [][2]string{{"GET", "/api/admins/3/token"}, {"POST", "/api/admins/3/token"}, {"DELETE", "/api/admins/3/token"}, {"GET", "/api/admins/1/token"}} {
		if c, _ := call(t, srv, bobC, r[0], r[1], ""); c != 403 {
			t.Errorf("bob %s %s: %d, want 403", r[0], r[1], c)
		}
	}
	if c, _ := bearer(t, srv, carolTok, "GET", "/api/me", ""); c != 200 {
		t.Error("bob's refused calls changed carol's token")
	}
	if c, _ := call(t, srv, rootC, "GET", "/api/admins/99/token", ""); c != 404 {
		t.Errorf("unknown admin: %d, want 404", c)
	}

	// Root can revoke carol's.
	if c, _ := call(t, srv, rootC, "DELETE", "/api/admins/3/token", ""); c != 200 {
		t.Fatal(c)
	}
	if c, _ := bearer(t, srv, carolTok, "GET", "/api/me", ""); c != 401 {
		t.Errorf("revoked: %d", c)
	}

	// Both ends are in the audit log.
	_, raw := call(t, srv, rootC, "GET", "/api/audit", "")
	for _, want := range []string{`"token.create"`, `"token.delete"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("audit lacks %s", want)
		}
	}
}

func TestTokenCannotCreateOrChangeLogins(t *testing.T) {
	srv, _ := newServer(t)
	rootC := loginJar(t, srv)
	call(t, srv, rootC, "POST", "/api/admins", `{"username":"bob","password":"bob-password"}`)
	tok := newToken(t, srv, rootC, 1)

	// Each of these would hand the holder of a root token a login of their own choosing.
	for _, r := range [][3]string{
		{"POST", "/api/admins", `{"username":"evil","password":"evil-password","root":true}`},
		{"PUT", "/api/admins/1", `{"password":"evil-password","root":true}`},
		{"PUT", "/api/admins/2", `{"password":"evil-password"}`},
		{"DELETE", "/api/admins/2", ""},
	} {
		if c, b := bearer(t, srv, tok, r[0], r[1], r[2]); c != 403 {
			t.Errorf("%s %s with a token: %d %s, want 403", r[0], r[1], c, b)
		}
	}
	if login(srv, "evil", "evil-password") == 200 {
		t.Fatal("a token created a login")
	}
	// The password of root is unchanged, and so is the token.
	if login(srv, "root", "right-password") != 200 {
		t.Error("root's password was changed through a token")
	}
	if c, b := bearer(t, srv, tok, "GET", "/api/admins", ""); c != 200 || !strings.Contains(b, `"bob"`) {
		t.Errorf("reading the admin list must still work: %d %s", c, b)
	}
	// The same calls still work with a session.
	if c, b := call(t, srv, rootC, "POST", "/api/admins", `{"username":"carol","password":"carol-password"}`); c != 200 {
		t.Errorf("session create: %d %s", c, b)
	}
}

func TestSessionWritesRefuseCrossOriginRequests(t *testing.T) {
	srv, _ := newServer(t)
	rootC := loginJar(t, srv)
	send := func(hdr map[string]string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/api/platforms", strings.NewReader(`{"name":"p","base_url":"https://a.example.com","token":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(rootC[0])
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if c := send(map[string]string{"Origin": "https://evil.example"}); c != 403 {
		t.Errorf("foreign Origin: %d, want 403", c)
	}
	if c := send(map[string]string{"Sec-Fetch-Site": "cross-site"}); c != 403 {
		t.Errorf("cross-site fetch: %d, want 403", c)
	}
	if c := send(map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": srv.URL}); c != 200 {
		t.Errorf("same-origin: %d, want 200", c)
	}
}

func TestSessionEndsOnLogoutAndOnPasswordChange(t *testing.T) {
	srv, _ := newServer(t)
	rootC := loginJar(t, srv)
	if c, _ := call(t, srv, rootC, "GET", "/api/me", ""); c != 200 {
		t.Fatalf("fresh session: %d", c)
	}

	// Logging out kills the server-side session, not just the cookie.
	other := loginJar(t, srv)
	if c, _ := call(t, srv, other, "POST", "/api/logout", ""); c != 200 {
		t.Fatalf("logout: %d", c)
	}
	if c, _ := call(t, srv, other, "GET", "/api/me", ""); c != 401 {
		t.Errorf("session after logout: %d, want 401", c)
	}

	// Changing an admin's password signs that admin out everywhere.
	if c, body := call(t, srv, rootC, "POST", "/api/admins", `{"username":"bob","password":"bob-password-1","root":false,"platform_ids":[]}`); c != 200 {
		t.Fatalf("create bob: %d %s", c, body)
	}
	bobC := loginAs(t, srv, "bob", "bob-password-1")
	if c, _ := call(t, srv, bobC, "GET", "/api/me", ""); c != 200 {
		t.Fatalf("bob session: %d", c)
	}
	if c, body := call(t, srv, rootC, "PUT", "/api/admins/2", `{"username":"bob","password":"bob-password-2","root":false,"platform_ids":[]}`); c != 200 {
		t.Fatalf("change bob: %d %s", c, body)
	}
	if c, _ := call(t, srv, bobC, "GET", "/api/me", ""); c != 401 {
		t.Errorf("bob's session after a password change: %d, want 401", c)
	}
}

func TestSuccessfulLoginForgetsTheUsersFailures(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h, _ := bcrypt.GenerateFromPassword([]byte("right-password"), bcrypt.MinCost)
	if _, err := st.CreateAdmin("root", string(h), true, false, nil); err != nil {
		t.Fatal(err)
	}
	s := New(st, manager.New(t.Context(), st, zap.NewNop(), time.Millisecond))
	for range maxLoginFails - 1 {
		s.recordFail("user:root")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"root","password":"right-password"}`))
	req.Header.Set("Content-Type", "application/json")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	if s.fails.Get("user:root") != nil {
		t.Error("a successful login must clear the user's failure count")
	}
}

func loginAs(t *testing.T, srv *httptest.Server, user, pass string) []*http.Cookie {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/login", "application/json", strings.NewReader(`{"username":"`+user+`","password":"`+pass+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.Cookies()
}

func TestReadOnlyAdminReadsWhatItIsGrantedAndChangesNothing(t *testing.T) {
	srv, st := newServer(t)
	rootC := loginJar(t, srv)
	call(t, srv, rootC, "POST", "/api/platforms", `{"name":"p1","base_url":"https://a.example.com","token":"x"}`)
	call(t, srv, rootC, "POST", "/api/platforms", `{"name":"p2","base_url":"https://b.example.com","token":"x"}`)
	if c, b := call(t, srv, rootC, "POST", "/api/admins", `{"username":"eve","password":"eve-password","root":true,"read_only":true}`); c != 400 {
		t.Errorf("a read-only root: %d %s", c, b)
	}
	if c, b := call(t, srv, rootC, "POST", "/api/admins", `{"username":"eve","password":"eve-password","read_only":true,"platform_ids":[1]}`); c != 200 {
		t.Fatal(c, b)
	}
	if _, raw := call(t, srv, rootC, "GET", "/api/admins", ""); !strings.Contains(raw, `"read_only":true`) {
		t.Errorf("list: %s", raw)
	}
	eve := loginAs(t, srv, "eve", "eve-password")

	// Reads work, on the granted platform only.
	if c, b := call(t, srv, eve, "GET", "/api/me", ""); c != 200 || !strings.Contains(b, `"read_only":true`) {
		t.Errorf("me: %d %s", c, b)
	}
	if c, b := call(t, srv, eve, "GET", "/api/platforms/1/jobs", ""); c != 200 {
		t.Errorf("read granted: %d %s", c, b)
	}
	if c, _ := call(t, srv, eve, "GET", "/api/platforms/2/jobs", ""); c != 403 {
		t.Errorf("read ungranted: %d, want 403", c)
	}

	// Nothing changes, by session or by token.
	tok := newToken(t, srv, eve, 2)
	writes := [][3]string{
		{"POST", "/api/platforms/1/refresh", `{}`},
		{"POST", "/api/platforms/1/quota", `{"mode":"add","amount":1,"unit":"usd","group":"default"}`},
		{"POST", "/api/platforms/1/jobs", `{"name":"n","kind":"refresh","spec":"@every 1h","enabled":true}`},
		{"DELETE", "/api/platforms/1/jobs/1", ""},
		{"POST", "/api/platforms", `{"name":"p3","base_url":"https://c.example.com","token":"x"}`},
	}
	for _, w := range writes {
		if c, _ := call(t, srv, eve, w[0], w[1], w[2]); c != 403 {
			t.Errorf("session %s %s: %d, want 403", w[0], w[1], c)
		}
		if c, _ := bearer(t, srv, tok, w[0], w[1], w[2]); c != 403 {
			t.Errorf("token %s %s: %d, want 403", w[0], w[1], c)
		}
	}
	if c, b := bearer(t, srv, tok, "GET", "/api/platforms", ""); c != 200 || strings.Contains(b, "p2") {
		t.Errorf("token read: %d %s", c, b)
	}
	if isErr, b := toolText(t, srv, tok, "adjust_quota", `{"platform_id":1,"mode":"add","amount":1,"unit":"usd","group":"default"}`); !isErr || !strings.Contains(b, "只读") {
		t.Errorf("adjust_quota over MCP: %v %s", isErr, b)
	}
	if isErr, b := toolText(t, srv, tok, "list_jobs", `{"platform_id":1}`); isErr {
		t.Errorf("list_jobs over MCP: %s", b)
	}
	if runs, _ := st.Runs(1, 10); len(runs) != 0 {
		t.Errorf("a refused write left runs behind: %v", runs)
	}

	// It still manages its own token, and changing the flag takes effect at once.
	if c, _ := call(t, srv, eve, "DELETE", "/api/admins/2/token", ""); c != 200 {
		t.Errorf("own token: %d", c)
	}
	call(t, srv, rootC, "PUT", "/api/admins/2", `{"username":"eve","platform_ids":[1]}`)
	if c, _ := call(t, srv, loginAs(t, srv, "eve", "eve-password"), "POST", "/api/platforms/1/jobs", `{"name":"n","kind":"refresh","spec":"@every 1h","enabled":true}`); c != 200 {
		t.Errorf("after the flag is cleared: %d, want 200", c)
	}
}

func TestUsageEndpointFollowsGrantsAndReadOnly(t *testing.T) {
	srv, st := newServer(t)
	rootC := loginJar(t, srv)
	call(t, srv, rootC, "POST", "/api/platforms", `{"name":"p1","base_url":"https://a.example.com","token":"x"}`)
	call(t, srv, rootC, "POST", "/api/platforms", `{"name":"p2","base_url":"https://b.example.com","token":"x"}`)
	call(t, srv, rootC, "POST", "/api/admins", `{"username":"eve","password":"eve-password","read_only":true,"platform_ids":[1]}`)

	// h is 2026-01-01 23:00 UTC and h+3600 is 2026-01-02 00:00 UTC: two days in UTC, but in UTC+8
	// (07:00 and 08:00 on Jan 2) one day. The day bucket depends on tz.
	h := int64(1767308400)
	if err := st.SaveUsage(1, store.UsageState{CollectedTo: h + 3600, QuotaPerUnit: 500000},
		[]store.UsageHour{{T: h, Model: "gpt", Count: 2, Quota: 20, Tokens: 200}, {T: h + 3600, Model: "gpt", Count: 1, Quota: 5, Tokens: 50}},
		[]store.UserHour{{T: h, Username: "a", Count: 2, Quota: 20, Tokens: 200}}); err != nil {
		t.Fatal(err)
	}
	type resp struct {
		State  *store.UsageState
		Step   string
		Models []store.UsageHour
		Users  []store.UserHour
	}
	get := func(c []*http.Cookie, path string) (int, resp) {
		code, b := call(t, srv, c, "GET", path, "")
		var r resp
		_ = json.Unmarshal([]byte(b), &r)
		return code, r
	}
	q := "/api/platforms/1/usage?from=" + strconv.FormatInt(h-86400, 10) + "&to=" + strconv.FormatInt(h+86400, 10)

	c, r := get(rootC, q)
	if c != 200 || r.Step != "day" || r.State == nil || r.State.QuotaPerUnit != 500000 || len(r.Models) != 2 || r.Models[0].Quota != 20 || r.Models[1].Quota != 5 {
		t.Fatalf("UTC days: %d %+v", c, r)
	}
	// In UTC+8 both hours fall on Jan 2, which starts at Jan 1 16:00 UTC.
	if _, r = get(rootC, q+"&tz=480"); len(r.Models) != 1 || r.Models[0].Quota != 25 || r.Models[0].T != 1767283200 {
		t.Fatalf("UTC+8 day: %+v", r)
	}
	// h-8h is Jan 1 15:00 UTC: Jan 1 in UTC, but 23:00 on Jan 1 in UTC+8, a different day from h.
	if err := st.SaveUsage(1, store.UsageState{CollectedTo: h + 3600, QuotaPerUnit: 500000}, []store.UsageHour{{T: h - 8*3600, Model: "gpt", Count: 1, Quota: 7, Tokens: 70}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, r = get(rootC, q+"&tz=480"); len(r.Models) != 2 || r.Models[0].T != 1767283200-86400 || r.Models[0].Quota != 7 || r.Models[1].Quota != 25 {
		t.Fatalf("UTC+8 split: %+v", r)
	}
	if _, r = get(rootC, q); len(r.Models) != 2 || r.Models[0].Quota != 27 || r.Models[1].Quota != 5 {
		t.Fatalf("UTC days with the extra hour: %+v", r)
	}
	if _, r = get(rootC, q+"&step=hour"); len(r.Models) != 3 || r.Models[1].T != h {
		t.Fatalf("hours: %+v", r)
	}
	if c, _ = get(rootC, "/api/platforms/1/usage?step=week"); c != 400 {
		t.Errorf("bad step: %d", c)
	}
	if c, _ = get(rootC, "/api/platforms/1/usage?step=hour&from=0&to=99999999"); c != 400 {
		t.Errorf("hourly span: %d", c)
	}
	if _, r = get(rootC, "/api/platforms/2/usage"); r.State != nil || len(r.Models) != 0 {
		t.Errorf("an uncollected platform: %+v", r)
	}

	// Read-only eve may read platform 1, not platform 2, and may not start a collection.
	eve := loginAs(t, srv, "eve", "eve-password")
	if c, _ = get(eve, q); c != 200 {
		t.Errorf("eve reads: %d", c)
	}
	if c, _ = get(eve, "/api/platforms/2/usage"); c != 403 {
		t.Errorf("eve reads p2: %d", c)
	}
	if c, _ := call(t, srv, eve, "POST", "/api/platforms/1/usage/collect", ""); c != 403 {
		t.Errorf("eve collects: %d", c)
	}
	tok := newToken(t, srv, eve, 2)
	if isErr, b := toolText(t, srv, tok, "get_usage", `{"platform_id":1,"from":`+strconv.FormatInt(h-86400, 10)+`,"to":`+strconv.FormatInt(h+86400, 10)+`}`); isErr || !strings.Contains(b, `"quota":20`) {
		t.Errorf("get_usage: %v %s", isErr, b)
	}
	if isErr, b := toolText(t, srv, tok, "collect_usage", `{"platform_id":1}`); !isErr || !strings.Contains(b, "只读") {
		t.Errorf("collect_usage: %v %s", isErr, b)
	}
}
