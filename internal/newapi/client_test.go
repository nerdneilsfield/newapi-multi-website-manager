package newapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAllUsersDedupesAndSkipsDeleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Page 2 repeats user 100 (the list shifted while paging) and has a soft-deleted user.
		page := r.URL.Query().Get("p")
		items := []map[string]any{}
		switch page {
		case "1":
			for i := 1; i <= 100; i++ {
				items = append(items, map[string]any{"id": i})
			}
		case "2":
			items = append(items, map[string]any{"id": 100}, map[string]any{"id": 101}, map[string]any{"id": 102, "DeletedAt": map[string]any{"Valid": true}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items, "total": 103}})
	}))
	defer srv.Close()
	us, err := New(srv.URL, "t", time.Millisecond).AllUsers(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(us) != 101 {
		t.Fatalf("got %d users, want 101", len(us))
	}
}

func TestRedirectIsNotFollowedAndWriteNotRetriedOn429(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/api/user/manage" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		http.Redirect(w, r, "http://127.0.0.1:1/steal", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := New(srv.URL, "t", time.Millisecond)
	if _, err := c.Groups(context.Background()); err == nil {
		t.Fatal("redirect should be an error")
	}
	hits = 0
	if err := c.Adjust(context.Background(), 1, "add", 5); err == nil || hits != 1 {
		t.Fatalf("write must not be retried: err=%v hits=%d", err, hits)
	}
}
