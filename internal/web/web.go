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

	mu       sync.Mutex
	sessions map[string]session // in memory: a restart logs everyone out
	fails    map[string]loginFails
}

type loginFails struct {
	n     int
	until time.Time
}

const maxLoginFails = 5 // per client IP and per username, then locked for loginLockout

var loginLockout = time.Minute

type session struct {
	adminID int64
	expires time.Time
}

// New builds a Server.
func New(s *store.Store, m *manager.Manager) *Server {
	return &Server{Store: s, Mgr: m, sessions: map[string]session{}, fails: map[string]loginFails{}}
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
		writeJSON(w, map[string]any{"id": me.ID, "username": me.Username, "root": me.IsRoot})
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
	mux.HandleFunc("GET /api/platforms/{id}/jobs", s.platform(s.listJobs))
	mux.HandleFunc("POST /api/platforms/{id}/jobs", s.platform(s.saveJob))
	mux.HandleFunc("PUT /api/platforms/{id}/jobs/{job}", s.platform(s.saveJob))
	mux.HandleFunc("DELETE /api/platforms/{id}/jobs/{job}", s.platform(s.deleteJob))

	mux.HandleFunc("GET /api/audit", s.auth(false, s.listAudit))

	mux.HandleFunc("GET /api/admins", s.auth(true, s.listAdmins))
	mux.HandleFunc("POST /api/admins", s.auth(true, s.saveAdmin))
	mux.HandleFunc("PUT /api/admins/{id}", s.auth(true, s.saveAdmin))
	mux.HandleFunc("DELETE /api/admins/{id}", s.auth(true, s.deleteAdmin))
	return secure(mux)
}

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

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

// sameOriginJSON lets a state-changing request through only if it is JSON and same-origin,
// which blocks cross-site form posts.
func sameOriginJSON(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") && r.ContentLength != 0 {
		fail(w, http.StatusUnsupportedMediaType, "需要 application/json")
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err != nil || u.Host != r.Host {
			fail(w, http.StatusForbidden, "跨站请求被拒绝")
			return false
		}
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

// auth requires a valid session; root additionally requires a root admin.
func (s *Server) auth(root bool, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && !sameOriginJSON(w, r) {
			return
		}
		c, err := r.Cookie(cookieName)
		if err != nil {
			fail(w, http.StatusUnauthorized, "未登录")
			return
		}
		s.mu.Lock()
		sess, ok := s.sessions[c.Value]
		if ok && time.Now().After(sess.expires) {
			delete(s.sessions, c.Value)
			ok = false
		}
		s.mu.Unlock()
		if !ok {
			fail(w, http.StatusUnauthorized, "未登录")
			return
		}
		me, err := s.Store.Admin(sess.adminID) // re-read so revoked grants apply immediately
		if err != nil {
			fail(w, http.StatusUnauthorized, "未登录")
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
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, f := range s.fails {
		if now.After(f.until) {
			delete(s.fails, k)
		}
	}
	for _, k := range keys {
		if f := s.fails[k]; f.n >= maxLoginFails {
			return true
		}
	}
	return false
}

func (s *Server) recordFail(keys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		f := s.fails[k]
		f.n++
		f.until = time.Now().Add(loginLockout)
		s.fails[k] = f
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !sameOriginJSON(w, r) {
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
	s.mu.Lock()
	delete(s.fails, "user:"+in.Username)
	for k, v := range s.sessions {
		if time.Now().After(v.expires) {
			delete(s.sessions, k)
		}
	}
	s.sessions[tok] = session{a.ID, time.Now().Add(sessionTTL)}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: int(sessionTTL.Seconds()),
	})
	s.audit(a, 0, "", "login", map[string]any{})
	writeJSON(w, map[string]any{"username": a.Username, "root": a.IsRoot})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !sameOriginJSON(w, r) {
		return
	}
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
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
		fail(w, 400, "kind 必须是 refresh 或 quota")
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
	PlatformIDs []int64 `json:"platform_ids"`
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
		out = append(out, adminView{a.ID, a.Username, a.IsRoot, a.PlatformIDs})
	}
	writeJSON(w, out)
}

func (s *Server) saveAdmin(w http.ResponseWriter, r *http.Request, me store.Admin) {
	var in struct {
		Username    string  `json:"username"`
		Password    string  `json:"password"`
		Root        bool    `json:"root"`
		PlatformIDs []int64 `json:"platform_ids"`
	}
	if !readJSON(w, r, &in) {
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
		if _, err := s.Store.CreateAdmin(in.Username, hash, in.Root, in.PlatformIDs); err != nil {
			fail(w, 400, "创建失败(用户名可能重复): "+err.Error())
			return
		}
		s.audit(me, 0, "", "admin.create", map[string]any{"username": in.Username, "root": in.Root, "platform_ids": in.PlatformIDs})
	} else {
		if id == me.ID && !in.Root {
			fail(w, 400, "不能取消自己的 root 权限")
			return
		}
		if err := s.Store.UpdateAdmin(id, hash, in.Root, in.PlatformIDs); err != nil {
			fail(w, 400, "保存失败: "+err.Error())
			return
		}
		s.dropSessions(id)
		target, _ := s.Store.Admin(id)
		s.audit(me, 0, "", "admin.update", map[string]any{"username": target.Username, "root": in.Root, "platform_ids": in.PlatformIDs, "password_changed": hash != ""})
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
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.sessions {
		if v.adminID == adminID {
			delete(s.sessions, k)
		}
	}
}
