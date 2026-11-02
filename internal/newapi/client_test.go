package newapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestReadIsRetriedOn429ThenGivesUp(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"data":["a"]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "t", time.Millisecond)
	start := time.Now()
	if _, err := c.Groups(context.Background()); err != nil || hits != 3 {
		t.Fatalf("err=%v hits=%d, want success on the third try", err, hits)
	}
	if d := time.Since(start); d < 2*time.Second {
		t.Fatalf("Retry-After was not honoured: %v", d)
	}

	hits = -100 // never succeeds
	err := func() error { _, err := c.Groups(context.Background()); return err }()
	if err == nil || !strings.Contains(err.Error(), "429") || hits != -100+4 {
		t.Fatalf("err=%v hits=%d, want HTTP 429 after 1+3 tries", err, hits)
	}
}

func TestRetryWithoutRetryAfterWaitsInsteadOfHammering(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := New(srv.URL, "t", time.Millisecond).Groups(ctx); err == nil || hits != 1 {
		t.Fatalf("err=%v hits=%d, want one try and then a wait that outlasts the deadline", err, hits)
	}
}

func TestRequestsAreSpacedByInterval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "t", 200*time.Millisecond)
	start := time.Now()
	for range 3 {
		if _, err := c.Groups(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Fatalf("3 requests took %v, want at least 2 intervals", d)
	}
}

func TestRetryKeepsTheIntervalToo(t *testing.T) {
	var at []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		at = append(at, time.Now())
		if len(at) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "t", 300*time.Millisecond).Groups(context.Background()); err != nil || len(at) != 2 {
		t.Fatalf("err=%v attempts=%d", err, len(at))
	}
	if d := at[1].Sub(at[0]); d < 250*time.Millisecond {
		t.Fatalf("retry came %v after the first attempt, want the 300ms interval", d)
	}
}

func TestRequestIsNotSentWhenTheIntervalOutlastsItsDeadline(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "t", time.Second)
	if _, err := c.Groups(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Groups(ctx); err == nil || hits != 1 {
		t.Fatalf("err=%v hits=%d, want the second request refused without being sent", err, hits)
	}
}
