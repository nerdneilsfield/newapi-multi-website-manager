// Package web serves the management UI and its JSON API.
package web

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"github.com/unrolled/secure"
	"golang.org/x/crypto/bcrypt"

	"github.com/nerdneilsfield/newapi-multi-website-manager/internal/manager"
	"github.com/nerdneilsfield/newapi-multi-website-manager/internal/newapi"
	"github.com/nerdneilsfield/newapi-multi-website-manager/internal/store"
)

//go:embed index.html
var indexHTML []byte

const (
	cookieName = "napm_session"
	sessionTTL = 7 * 24 * time.Hour
)

// Server wires HTTP handlers to the store and manager.
type Server struct {
	Store *store.Store
	Mgr   *manager.Manager

	mu       sync.Mutex                     // makes read-then-write on fails atomic
	sessions *ttlcache.Cache[string, int64] // token -> admin id; in memory, so a restart logs everyone out
	fails    *ttlcache.Cache[string, int]   // "ip:..." and "user:..." -> failed logins, kept for loginLockout
	csrf     *http.CrossOriginProtection
}

const maxLoginFails = 5 // per client IP and per username, then locked for loginLockout

var loginLockout = time.Minute

// New builds a Server.
func New(s *store.Store, m *manager.Manager) *Server {
	return &Server{
		Store: s, Mgr: m, csrf: http.NewCrossOriginProtection(),
		sessions: ttlcache.New(ttlcache.WithTTL[string, int64](sessionTTL), ttlcache.WithDisableTouchOnHit[string, int64]()),
		fails:    ttlcache.New(ttlcache.WithDisableTouchOnHit[string, int]()),
	}
}

type handler func(w http.ResponseWriter, r *http.Request, me store.Admin)

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/me", s.auth(false, func(w http.ResponseWriter, _ *http.Request, me store.Admin) {
		writeJSON(w, map[string]any{"id": me.ID, "username": me.Username, "root": me.IsRoot, "read_only": me.ReadOnly, "has_token": me.APIToken != ""})
	}))

	mux.HandleFunc("GET /api/platforms", s.auth(false, s.listPlatforms))
	mux.HandleFunc("POST /api/platforms", s.auth(true, s.savePlatform))
	mux.HandleFunc("PUT /api/platforms/{id}", s.auth(true, s.savePlatform))
	mux.HandleFunc("DELETE /api/platforms/{id}", s.auth(true, s.deletePlatform))

	mux.HandleFunc("GET /api/platforms/{id}/stats", s.platform(s.stats))
	mux.HandleFunc("POST /api/platforms/{id}/refresh", s.platform(s.refresh))
	mux.HandleFunc("GET /api/platforms/{id}/users", s.platform(s.users))
	mux.HandleFunc("POST /api/platforms/{id}/quota", s.platform(s.quota))
	mux.HandleFunc("GET /api/platforms/{id}/runs", s.platform(s.runs))
	mux.HandleFunc("GET /api/platforms/{id}/usage", s.platform(s.usage))
	mux.HandleFunc("POST /api/platforms/{id}/usage/collect", s.platform(s.collectUsage))
	mux.HandleFunc("GET /api/platforms/{id}/jobs", s.platform(s.listJobs))
	mux.HandleFunc("POST /api/platforms/{id}/jobs", s.platform(s.saveJob))
	mux.HandleFunc("PUT /api/platforms/{id}/jobs/{job}", s.platform(s.saveJob))
	mux.HandleFunc("DELETE /api/platforms/{id}/jobs/{job}", s.platform(s.deleteJob))

	mux.HandleFunc("GET /api/audit", s.auth(false, s.listAudit))

	mux.HandleFunc("GET /api/admins/{id}/token", s.sessionAuth(false, s.tokenOf(s.getToken)))
	mux.HandleFunc("POST /api/admins/{id}/token", s.sessionAuth(false, s.tokenOf(s.newToken)))
	mux.HandleFunc("DELETE /api/admins/{id}/token", s.sessionAuth(false, s.tokenOf(s.deleteToken)))
	mux.Handle("/mcp", s.mcpHandler(mux))

	mux.HandleFunc("GET /api/admins", s.auth(true, s.listAdmins))
	mux.HandleFunc("POST /api/admins", s.sessionAuth(true, s.saveAdmin))
	mux.HandleFunc("PUT /api/admins/{id}", s.sessionAuth(true, s.saveAdmin))
	mux.HandleFunc("DELETE /api/admins/{id}", s.sessionAuth(true, s.deleteAdmin))
	return secureHeaders.Handler(mux)
}

var secureHeaders = secure.New(secure.Options{FrameDeny: true, ContentTypeNosniff: true, ReferrerPolicy: "no-referrer"})

// ---- helpers ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "请求格式错误")
		return false
	}
	return true
}

func pathID(r *http.Request, name string) int64 {
	id, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id
}

// sameOriginJSON lets a state-changing request through only if it is JSON and not cross-origin,
// which blocks cross-site form posts. The origin check is the standard library's.
func (s *Server) sameOriginJSON(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") && r.ContentLength != 0 {
		fail(w, http.StatusUnsupportedMediaType, "需要 application/json")
		return false
	}
	if err := s.csrf.Check(r); err != nil {
		fail(w, http.StatusForbidden, "跨站请求被拒绝")
		return false
	}
	return true
}

// auditLimit is the page size of GET /api/audit; ?before=<id> returns the next older page.
const auditLimit = 200

// audit records an action after it succeeded. A failed write must not undo or hide the action
// itself, so the error is dropped like the other best-effort writes here.
func (s *Server) audit(me store.Admin, platformID int64, platform, action string, detail map[string]any) {
	_ = s.Store.AddAudit(me, platformID, platform, action, detail)
}

// auditP is audit for an action on an existing platform whose name is looked up.
func (s *Server) auditP(me store.Admin, platformID int64, action string, detail map[string]any) {
	p, _ := s.Store.Platform(platformID)
	s.audit(me, platformID, p.Name, action, detail)
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, me store.Admin) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	es, err := s.Store.Audits(me, before, auditLimit)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, es)
}

// authenticate identifies the caller: by the Authorization header when there is one, otherwise
// by the session cookie. A bad token never falls back to the cookie. It writes the error itself.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (store.Admin, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		// A token is not an ambient credential, so the cross-site checks below do not apply to it.
		me, ok := s.byToken(h)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			fail(w, http.StatusUnauthorized, "令牌无效")
		}
		return me, ok
	}
	if r.Method != http.MethodGet && !s.sameOriginJSON(w, r) {
		return store.Admin{}, false
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		fail(w, http.StatusUnauthorized, "未登录")
		return store.Admin{}, false
	}
	sess := s.sessions.Get(c.Value)
	if sess == nil {
		fail(w, http.StatusUnauthorized, "未登录")
		return store.Admin{}, false
	}
	me, err := s.Store.Admin(sess.Value()) // re-read so revoked grants apply immediately
	if err != nil {
		fail(w, http.StatusUnauthorized, "未登录")
		return store.Admin{}, false
	}
	return me, true
}

// auth requires a valid session or token; root additionally requires a root admin. A read-only
// admin gets through only for GET, so no handler has to remember to refuse it.
func (s *Server) auth(root bool, h handler) http.HandlerFunc {
	return s.authAny(root, func(w http.ResponseWriter, r *http.Request, me store.Admin) {
		if me.ReadOnly && r.Method != http.MethodGet {
			fail(w, http.StatusForbidden, "只读账号不能修改数据")
			return
		}
		h(w, r, me)
	})
}

// authAny is auth without the read-only check, for what a read-only admin may still do: manage
// their own token.
func (s *Server) authAny(root bool, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		me, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		if root && !me.IsRoot {
			fail(w, http.StatusForbidden, "需要 root 管理员")
			return
		}
		h(w, r, me)
	}
}

// platform is auth plus a check that the admin may use the {id} platform.
func (s *Server) platform(h handler) http.HandlerFunc {
	return s.auth(false, func(w http.ResponseWriter, r *http.Request, me store.Admin) {
		if !s.Store.CanAccess(me, pathID(r, "id")) {
			fail(w, http.StatusForbidden, "无权访问该平台")
			return
		}
		h(w, r, me)
	})
}

// ---- login ----

// lockedOut reports whether any key has hit the failure limit. Keys are the client IP and the
// username, so one IP cannot guess many passwords and many IPs cannot hammer one account.
func (s *Server) lockedOut(keys ...string) bool {
	for _, k := range keys {
		if f := s.fails.Get(k); f != nil && f.Value() >= maxLoginFails {
			return true
		}
	}
	return false
}

func (s *Server) recordFail(keys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		n := 0
		if f := s.fails.Get(k); f != nil {
			n = f.Value()
		}
		s.fails.Set(k, n+1, loginLockout)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.sameOriginJSON(w, r) {
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	keys := []string{"ip:" + ip, "user:" + in.Username}
	if s.lockedOut(keys...) {
		fail(w, http.StatusTooManyRequests, "尝试次数过多，请稍后再试")
		return
	}
	a, err := s.Store.AdminByName(in.Username)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(a.PassHash), []byte(in.Password)) != nil {
		s.recordFail(keys...)
		fail(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		fail(w, http.StatusInternalServerError, "无法生成会话")
		return
	}
	tok := hex.EncodeToString(b)
	s.fails.Delete("user:" + in.Username)
	s.sessions.DeleteExpired()
	s.fails.DeleteExpired()
	s.sessions.Set(tok, a.ID, ttlcache.DefaultTTL)
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: int(sessionTTL.Seconds()),
	})
	s.audit(a, 0, "", "login", map[string]any{})
	writeJSON(w, map[string]any{"username": a.Username, "root": a.IsRoot})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !s.sameOriginJSON(w, r) {
		return
	}
	if c, err := r.Cookie(cookieName); err == nil {
		s.sessions.Delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1})
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- platforms ----

type platformView struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
}

func (s *Server) listPlatforms(w http.ResponseWriter, _ *http.Request, me store.Admin) {
	ps, err := s.Store.Platforms(me)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	out := make([]platformView, 0, len(ps))
	for _, p := range ps {
		out = append(out, platformView{p.ID, p.Name, p.BaseURL})
	}
	writeJSON(w, out)
}

func (s *Server) savePlatform(w http.ResponseWriter, r *http.Request, me store.Admin) {
	var in struct {
		Name    string `json:"name"`
		BaseURL string `json:"base_url"`
		Token   string `json:"token"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	u, err := url.Parse(strings.TrimSpace(in.BaseURL))
	if in.Name == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		fail(w, 400, "需要名称和以 http(s):// 开头的 URL(不能带用户名密码)")
		return
	}
	p := store.Platform{ID: pathID(r, "id"), Name: in.Name, BaseURL: strings.TrimRight(u.String(), "/"), Token: strings.TrimSpace(in.Token)}
	action := "platform.update"
	if p.ID == 0 {
		action = "platform.create"
		if p.Token == "" {
			fail(w, 400, "新平台需要 token")
			return
		}
		id, err := s.Store.CreatePlatform(p)
		if err != nil {
			fail(w, 400, "保存失败(名称可能重复): "+err.Error())
			return
		}
		p.ID = id
	} else if err := s.Store.UpdatePlatform(p); err != nil {
		fail(w, 400, "保存失败: "+err.Error())
		return
	}
	s.audit(me, p.ID, p.Name, action, map[string]any{"name": p.Name, "base_url": p.BaseURL, "token_changed": p.Token != ""})
	writeJSON(w, platformView{p.ID, p.Name, p.BaseURL})
}

func (s *Server) deletePlatform(w http.ResponseWriter, r *http.Request, me store.Admin) {
	id := pathID(r, "id")
	old, _ := s.Store.Platform(id)
	if err := s.Store.DeletePlatform(id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	s.Mgr.Forget(id)
	_ = s.Mgr.Reload()
	s.audit(me, id, old.Name, "platform.delete", map[string]any{"name": old.Name})
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- statistics, users, quota ----

func (s *Server) stats(w http.ResponseWriter, r *http.Request, _ store.Admin) {
	id := pathID(r, "id")
	data, err := s.Store.Snapshot(id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		fail(w, 500, err.Error())
		return
	}
	runs, _ := s.Store.Runs(id, 1)
	var last *store.Run
	if len(runs) > 0 {
		last = &runs[0]
	}
	var stats json.RawMessage
	if len(data) > 0 {
		stats = data
	}
	writeJSON(w, map[string]any{"stats": stats, "last_run": last})
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request, me store.Admin) {
	id := pathID(r, "id")
	s.started(w, func(runID int64) { s.auditP(me, id, "refresh", map[string]any{"run_id": runID}) })(s.Mgr.Refresh(id, nil, "", me.Username))
}

func (s *Server) collectUsage(w http.ResponseWriter, r *http.Request, me store.Admin) {
	id := pathID(r, "id")
	s.started(w, func(runID int64) { s.auditP(me, id, "usage.collect", map[string]any{"run_id": runID}) })(s.Mgr.CollectUsage(id, nil, "", me.Username))
}

// maxHourlySpan caps an hourly request: a year of hourly rows per model is too much to send.
const maxHourlySpan = 14 * 24 * 3600

// usage returns the collected usage between from and to (Unix seconds, to excluded; default the
// last 30 days) as per-model and per-user rows, by "hour" or by local "day" (tz is minutes east of
// UTC). Insights are computed from these rows in the browser or by the MCP client.
func (s *Server) usage(w http.ResponseWriter, r *http.Request, _ store.Admin) {
	q, id := r.URL.Query(), pathID(r, "id")
	num := func(k string, def int64) int64 {
		if v, err := strconv.ParseInt(q.Get(k), 10, 64); err == nil {
			return v
		}
		return def
	}
	now := time.Now().Unix()
	to := num("to", now+1)
	from := num("from", to-30*24*3600)
	tz := int(num("tz", 0))
	day := q.Get("step") != "hour"
	if q.Get("step") != "" && q.Get("step") != "hour" && q.Get("step") != "day" {
		fail(w, 400, "step 必须是 hour 或 day")
		return
	}
	if from >= to || tz < -14*60 || tz > 14*60 {
		fail(w, 400, "时间范围或时区无效")
		return
	}
	if !day && to-from > maxHourlySpan {
		fail(w, 400, "按小时查询最多 14 天")
		return
	}
	var state *store.UsageState
	switch st, err := s.Store.UsageState(id); {
	case err == nil:
		state = &st
	case !errors.Is(err, sql.ErrNoRows):
		fail(w, 500, err.Error())
		return
	}
	models, err := s.Store.UsageModels(id, from, to, day, tz)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	users, err := s.Store.UsageUsers(id, from, to, day, tz)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"state": state, "from": from, "to": to, "step": map[bool]string{true: "day", false: "hour"}[day], "models": models, "users": users})
}

// started answers a run launch and calls onStart once the run exists.
func (s *Server) started(w http.ResponseWriter, onStart func(runID int64)) func(int64, error) {
	return func(runID int64, err error) {
		switch {
		case errors.Is(err, manager.ErrBusy):
			fail(w, http.StatusConflict, err.Error())
		case err != nil:
			fail(w, http.StatusBadRequest, err.Error())
		default:
			onStart(runID)
			writeJSON(w, map[string]int64{"run_id": runID})
		}
	}
}

func (s *Server) users(w http.ResponseWriter, r *http.Request, _ store.Admin) {
	p, err := s.Store.Platform(pathID(r, "id"))
	if err != nil {
		fail(w, 404, "平台不存在")
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := s.Mgr.Client(p).UsersPage(ctx, r.URL.Query().Get("group"), r.URL.Query().Get("keyword"), page)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	kept := []newapi.User{}
	for _, u := range res.Items {
		if !u.Deleted() {
			kept = append(kept, u)
		}
	}
	res.Items = kept
	writeJSON(w, res)
}

func (s *Server) quota(w http.ResponseWriter, r *http.Request, me store.Admin) {
	var plan store.QuotaPlan
	if !readJSON(w, r, &plan) {
		return
	}
	id := pathID(r, "id")
	s.started(w, func(runID int64) { s.auditP(me, id, "quota", map[string]any{"run_id": runID, "plan": plan}) })(s.Mgr.ApplyQuota(id, nil, "", me.Username, plan))
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request, _ store.Admin) {
	rs, err := s.Store.Runs(pathID(r, "id"), 50)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if rs == nil {
		rs = []store.Run{}
	}
	writeJSON(w, rs)
}

// ---- jobs ----

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request, _ store.Admin) {
	js, err := s.Store.Jobs(pathID(r, "id"))
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if js == nil {
		js = []store.Job{}
	}
	writeJSON(w, js)
}

func (s *Server) saveJob(w http.ResponseWriter, r *http.Request, me store.Admin) {
	var j store.Job
	if !readJSON(w, r, &j) {
		return
	}
	j.PlatformID = pathID(r, "id")
	j.ID = pathID(r, "job")
	if j.Name == "" {
		fail(w, 400, "需要任务名称")
		return
	}
	if err := manager.ValidateSpec(j.Spec); err != nil {
		fail(w, 400, "定时表达式无效(5 段 cron 或 @every 1h): "+err.Error())
		return
	}
	switch j.Kind {
	case "refresh":
		j.Plan = nil
	case "usage":
		j.Plan = nil
	case "quota":
		if j.Plan == nil {
			fail(w, 400, "需要 plan")
			return
		}
		if err := manager.ValidatePlan(*j.Plan); err != nil {
			fail(w, 400, err.Error())
			return
		}
	default:
		fail(w, 400, "kind 必须是 refresh、usage 或 quota")
		return
	}
	action := "job.update"
	if j.ID == 0 {
		action = "job.create"
		id, err := s.Store.CreateJob(j)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		j.ID = id
	} else {
		old, err := s.Store.Job(j.ID)
		if err != nil || old.PlatformID != j.PlatformID {
			fail(w, 404, "任务不存在")
			return
		}
		if err := s.Store.UpdateJob(j); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	_ = s.Mgr.Reload()
	s.auditP(me, j.PlatformID, action, map[string]any{"name": j.Name, "kind": j.Kind, "spec": j.Spec, "enabled": j.Enabled, "plan": j.Plan})
	writeJSON(w, j)
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request, me store.Admin) {
	j, err := s.Store.Job(pathID(r, "job"))
	if err != nil || j.PlatformID != pathID(r, "id") {
		fail(w, 404, "任务不存在")
		return
	}
	if err := s.Store.DeleteJob(j.ID); err != nil {
		fail(w, 500, err.Error())
		return
	}
	_ = s.Mgr.Reload()
	s.auditP(me, j.PlatformID, "job.delete", map[string]any{"name": j.Name})
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- admins ----

type adminView struct {
	ID          int64   `json:"id"`
	Username    string  `json:"username"`
	Root        bool    `json:"root"`
	ReadOnly    bool    `json:"read_only"`
	PlatformIDs []int64 `json:"platform_ids"`
	HasToken    bool    `json:"has_token"`
}

func (s *Server) listAdmins(w http.ResponseWriter, _ *http.Request, _ store.Admin) {
	as, err := s.Store.Admins()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	out := make([]adminView, 0, len(as))
	for _, a := range as {
		if a.PlatformIDs == nil {
			a.PlatformIDs = []int64{}
		}
		out = append(out, adminView{a.ID, a.Username, a.IsRoot, a.ReadOnly, a.PlatformIDs, a.APIToken != ""})
	}
	writeJSON(w, out)
}

func (s *Server) saveAdmin(w http.ResponseWriter, r *http.Request, me store.Admin) {
	var in struct {
		Username    string  `json:"username"`
		Password    string  `json:"password"`
		Root        bool    `json:"root"`
		ReadOnly    bool    `json:"read_only"`
		PlatformIDs []int64 `json:"platform_ids"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Root && in.ReadOnly {
		fail(w, 400, "root 不能是只读账号")
		return
	}
	id := pathID(r, "id")
	hash := ""
	if in.Password != "" {
		if len(in.Password) < 8 {
			fail(w, 400, "密码至少 8 位")
			return
		}
		b, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
		if err != nil {
			fail(w, 400, "密码过长(bcrypt 上限 72 字节)")
			return
		}
		hash = string(b)
	}
	if id == 0 {
		if in.Username == "" || hash == "" {
			fail(w, 400, "新管理员需要用户名和密码")
			return
		}
		if _, err := s.Store.CreateAdmin(in.Username, hash, in.Root, in.ReadOnly, in.PlatformIDs); err != nil {
			fail(w, 400, "创建失败(用户名可能重复): "+err.Error())
			return
		}
		s.audit(me, 0, "", "admin.create", map[string]any{"username": in.Username, "root": in.Root, "read_only": in.ReadOnly, "platform_ids": in.PlatformIDs})
	} else {
		if id == me.ID && !in.Root {
			fail(w, 400, "不能取消自己的 root 权限")
			return
		}
		if err := s.Store.UpdateAdmin(id, hash, in.Root, in.ReadOnly, in.PlatformIDs); err != nil {
			fail(w, 400, "保存失败: "+err.Error())
			return
		}
		s.dropSessions(id)
		target, _ := s.Store.Admin(id)
		s.audit(me, 0, "", "admin.update", map[string]any{"username": target.Username, "root": in.Root, "read_only": in.ReadOnly, "platform_ids": in.PlatformIDs, "password_changed": hash != ""})
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) deleteAdmin(w http.ResponseWriter, r *http.Request, me store.Admin) {
	id := pathID(r, "id")
	if id == me.ID {
		fail(w, 400, "不能删除自己")
		return
	}
	target, _ := s.Store.Admin(id)
	if err := s.Store.DeleteAdmin(id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	s.dropSessions(id)
	s.audit(me, 0, "", "admin.delete", map[string]any{"username": target.Username})
	writeJSON(w, map[string]bool{"ok": true})
}

// dropSessions logs an admin out everywhere after a password or privilege change.
func (s *Server) dropSessions(adminID int64) {
	for _, it := range s.sessions.Items() {
		if it.Value() == adminID {
			s.sessions.Delete(it.Key())
		}
	}
}
