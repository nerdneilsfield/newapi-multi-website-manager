// Package newapi is a minimal client for the admin API of a QuantumNous/new-api
// instance (v1.0.0-rc.22 or newer, authenticated with a PAT).
package newapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"golang.org/x/time/rate"
)

const pageSize = 100 // new-api caps page_size at 100

// User is the subset of new-api's user object the manager needs.
type User struct {
	ID           int64           `json:"id"`
	Username     string          `json:"username"`
	DisplayName  string          `json:"display_name"`
	Group        string          `json:"group"`
	Quota        int64           `json:"quota"`
	UsedQuota    int64           `json:"used_quota"`
	RequestCount int64           `json:"request_count"`
	Status       int             `json:"status"`
	Role         int             `json:"role"`
	DeletedAt    json.RawMessage `json:"DeletedAt,omitempty"`
}

// Deleted reports whether the user is soft-deleted; search/list return those too.
func (u User) Deleted() bool {
	s := string(u.DeletedAt)
	return s != "" && s != "null"
}

// Page is one page of users.
type Page struct {
	Items []User `json:"items"`
	Total int    `json:"total"`
}

// Client talks to one platform. Calls are spaced by Interval so that bulk work
// stays under new-api's default global limit (360 requests / 180 s / IP).
type Client struct {
	base, token string
	hc          *retryablehttp.Client
}

// New returns a client for baseURL (no trailing slash needed).
func New(baseURL, token string, interval time.Duration) *Client {
	hc := retryablehttp.NewClient()
	hc.Logger = nil
	hc.RetryMax = 3
	hc.RetryWaitMax = 300 * time.Second
	// Only reads are retried, and only on 429: behind a proxy a 429 on a write may come after it took effect.
	hc.CheckRetry = func(_ context.Context, resp *http.Response, _ error) (bool, error) {
		return resp != nil && resp.StatusCode == http.StatusTooManyRequests && resp.Request.Method == http.MethodGet, nil
	}
	hc.Backoff = func(lo, hi time.Duration, attempt int, resp *http.Response) time.Duration {
		if resp != nil && resp.Header.Get("Retry-After") != "" { // seconds or HTTP-date, parsed by the library
			return min(retryablehttp.DefaultBackoff(lo, hi, attempt, resp), hi)
		}
		return 10 * time.Second
	}
	hc.ErrorHandler = retryablehttp.PassthroughErrorHandler // give up with the last response, so its status is reported
	gap := rate.NewLimiter(rate.Inf, 1)
	if interval > 0 {
		gap = rate.NewLimiter(rate.Every(interval), 1)
	}
	hc.HTTPClient = &http.Client{
		Timeout:   30 * time.Second,
		Transport: spaced{gap: gap, next: http.DefaultTransport},
		// A redirect would resend the PAT to wherever it points (even http or another port).
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), token: token, hc: hc}
}

// spaced makes every attempt, retries included, wait for the limiter. When the permit cannot be
// had before the request's deadline, rate.Limiter.Wait fails at once and the request is not sent.
type spaced struct {
	gap  *rate.Limiter
	next http.RoundTripper
}

func (t spaced) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := t.gap.Wait(r.Context()); err != nil {
		return nil, err
	}
	return t.next.RoundTrip(r)
}

// Same reports whether c was built for this base URL and token.
func (c *Client) Same(baseURL, token string) bool {
	return c.base == strings.TrimRight(baseURL, "/") && c.token == token
}

// QuotaPerUnit returns how many quota units equal one currency unit.
func (c *Client) QuotaPerUnit(ctx context.Context) (float64, error) {
	var d struct {
		QuotaPerUnit float64 `json:"quota_per_unit"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/status", nil, nil, &d); err != nil {
		return 0, err
	}
	if d.QuotaPerUnit <= 0 {
		return 0, fmt.Errorf("/api/status returned no quota_per_unit")
	}
	return d.QuotaPerUnit, nil
}

// Groups returns the group names configured on the platform (needs group:read).
func (c *Client) Groups(ctx context.Context) ([]string, error) {
	var names []string
	err := c.do(ctx, http.MethodGet, "/api/group/", nil, nil, &names)
	return names, err
}

// UsersPage fetches one page (1-based) of users, optionally filtered.
func (c *Client) UsersPage(ctx context.Context, group, keyword string, page int) (Page, error) {
	// sort_by is ignored by releases that predate it; AllUsers also dedupes by id.
	q := url.Values{
		"group": {group}, "keyword": {keyword}, "p": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(pageSize)},
		"sort_by": {"id"}, "sort_order": {"asc"},
	}
	var p Page
	err := c.do(ctx, http.MethodGet, "/api/user/search", q, nil, &p)
	return p, err
}

// AllUsers returns every non-deleted user in group ("" = all groups).
func (c *Client) AllUsers(ctx context.Context, group string) ([]User, error) {
	var out []User
	seen := map[int64]bool{}
	for page := 1; ; page++ {
		p, err := c.UsersPage(ctx, group, "", page)
		if err != nil {
			return nil, err
		}
		for _, u := range p.Items {
			if !u.Deleted() && !seen[u.ID] {
				seen[u.ID] = true
				out = append(out, u)
			}
		}
		if len(p.Items) == 0 || page*pageSize >= p.Total {
			return out, nil
		}
	}
}

// User fetches a single user.
func (c *Client) User(ctx context.Context, id int64) (User, error) {
	var u User
	err := c.do(ctx, http.MethodGet, "/api/user/"+strconv.FormatInt(id, 10), nil, nil, &u)
	return u, err
}

// Adjust changes a user's balance. mode is add, subtract or override; value is raw quota.
func (c *Client) Adjust(ctx context.Context, id int64, mode string, value int64) error {
	body := map[string]any{"id": id, "action": "add_quota", "mode": mode, "value": value}
	return c.do(ctx, http.MethodPost, "/api/user/manage", nil, body, nil)
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	target := c.base + path
	if q != nil {
		target += "?" + q.Encode()
	}
	req, err := retryablehttp.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(data))
	}
	var env struct {
		Success bool            `json:"success"`
		Message json.RawMessage `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("unexpected response (is the URL a new-api site?): %s", snippet(data))
	}
	if !env.Success {
		var s string
		if json.Unmarshal(env.Message, &s) != nil {
			s = string(env.Message)
		}
		return fmt.Errorf("%s", s)
	}
	if out != nil {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
