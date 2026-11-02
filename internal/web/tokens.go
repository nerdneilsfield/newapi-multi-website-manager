package web

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/nerdneilsfield/newapi-multi-website-manager/internal/store"
)

// byToken resolves an "Authorization: Bearer <token>" header to the admin that owns the token.
// The admin is read fresh, so a changed grant or a deleted admin applies at once.
func (s *Server) byToken(header string) (store.Admin, bool) {
	scheme, tok, ok := strings.Cut(header, " ")
	tok = strings.TrimSpace(tok)
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return store.Admin{}, false
	}
	a, err := s.Store.AdminByToken(tok)
	return a, err == nil
}

// sessionAuth is auth for endpoints that create, change or remove a credential: the API token
// and admin accounts. A token is refused there, so a leaked one cannot mint a login of its own
// (a new root, or a new password for an existing admin) or swap itself for another token.
func (s *Server) sessionAuth(root bool, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			fail(w, http.StatusForbidden, "令牌不能管理令牌和管理员账号，请登录网页后操作")
			return
		}
		s.authAny(root, h)(w, r) // a read-only admin may still manage their own token
	}
}

// tokenOf runs h for the admin whose token the path names. Everyone may manage their own token;
// root may manage anyone's, and so can read it.
func (s *Server) tokenOf(h func(w http.ResponseWriter, me, target store.Admin)) handler {
	return func(w http.ResponseWriter, r *http.Request, me store.Admin) {
		id := pathID(r, "id")
		if !me.IsRoot && me.ID != id {
			fail(w, http.StatusForbidden, "只能管理自己的令牌")
			return
		}
		target, err := s.Store.Admin(id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			fail(w, http.StatusNotFound, "管理员不存在")
		case err != nil:
			fail(w, http.StatusInternalServerError, err.Error())
		default:
			h(w, me, target)
		}
	}
}

func (s *Server) getToken(w http.ResponseWriter, _, target store.Admin) {
	writeJSON(w, map[string]string{"token": target.APIToken})
}

// newToken gives the admin a fresh token; the old one stops working at once.
func (s *Server) newToken(w http.ResponseWriter, me, target store.Admin) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		fail(w, http.StatusInternalServerError, "无法生成令牌")
		return
	}
	tok := "napm_" + hex.EncodeToString(b)
	if err := s.Store.SetAPIToken(target.ID, tok); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(me, 0, "", "token.create", map[string]any{"username": target.Username})
	writeJSON(w, map[string]string{"token": tok})
}

func (s *Server) deleteToken(w http.ResponseWriter, me, target store.Admin) {
	if err := s.Store.SetAPIToken(target.ID, ""); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(me, 0, "", "token.delete", map[string]any{"username": target.Username})
	writeJSON(w, map[string]bool{"ok": true})
}
