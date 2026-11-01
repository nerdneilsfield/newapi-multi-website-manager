// Package manager runs statistics refreshes and balance adjustments against
// platforms, either on demand or from cron schedules.
package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"

	"github.com/nerdneilsfield/newapi-multi-website-manager/internal/newapi"
	"github.com/nerdneilsfield/newapi-multi-website-manager/internal/store"
)

// maxWalletQuota is new-api's balance ceiling (1<<53 - 1).
const maxWalletQuota = 1<<53 - 1

// ErrBusy means another run is already in progress on the platform.
var ErrBusy = errors.New("该平台有任务正在运行")

// GroupStat aggregates one user group.
type GroupStat struct {
	Name     string `json:"name"`
	Users    int    `json:"users"`
	Quota    int64  `json:"quota"`
	Used     int64  `json:"used"`
	Requests int64  `json:"requests"`
}

// Stats is the snapshot saved by a refresh.
type Stats struct {
	At           int64       `json:"at"`
	QuotaPerUnit float64     `json:"quota_per_unit"`
	Users        int         `json:"users"`
	Quota        int64       `json:"quota"`
	Used         int64       `json:"used"`
	Requests     int64       `json:"requests"`
	Groups       []GroupStat `json:"groups"`
}

// Manager owns platform clients, the per-platform run lock and the scheduler.
type Manager struct {
	Store    *store.Store
	Log      *zap.Logger
	Interval time.Duration // minimum gap between requests to one platform

	ctx     context.Context
	wg      sync.WaitGroup
	mu      sync.Mutex
	clients map[int64]*newapi.Client
	busy    map[int64]bool
	cron    *cron.Cron
}

// New creates a manager; runs are canceled when ctx is done.
func New(ctx context.Context, s *store.Store, log *zap.Logger, interval time.Duration) *Manager {
	return &Manager{
		Store: s, Log: log, Interval: interval, ctx: ctx,
		clients: map[int64]*newapi.Client{}, busy: map[int64]bool{},
	}
}

// Client returns the (cached) client for a platform so its request spacing is shared.
func (m *Manager) Client(p store.Platform) *newapi.Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clients[p.ID]; ok && c.Same(p.BaseURL, p.Token) {
		return c
	}
	c := newapi.New(p.BaseURL, p.Token, m.Interval)
	m.clients[p.ID] = c
	return c
}

// Forget drops the cached client (and its PAT) for a deleted platform.
func (m *Manager) Forget(platformID int64) {
	m.mu.Lock()
	delete(m.clients, platformID)
	m.mu.Unlock()
}

// Wait blocks until all in-flight runs have finished.
func (m *Manager) Wait() { m.wg.Wait() }

// ---- scheduling ----

// ValidateSpec checks a cron expression (5 fields or @every/@daily style).
func ValidateSpec(spec string) error {
	_, err := cron.ParseStandard(spec)
	return err
}

// Reload rebuilds the scheduler from the enabled jobs in the store.
func (m *Manager) Reload() error {
	jobs, err := m.Store.EnabledJobs()
	if err != nil {
		return err
	}
	c := cron.New()
	for _, j := range jobs {
		id := j.ID
		if _, err := c.AddFunc(j.Spec, func() { m.runJob(id) }); err != nil {
			m.Log.Warn("skip job with bad schedule", zap.Int64("job", id), zap.Error(err))
		}
	}
	m.mu.Lock()
	old := m.cron
	m.cron = c
	m.mu.Unlock()
	if old != nil {
		old.Stop()
	}
	c.Start()
	return nil
}

// Stop halts the scheduler.
func (m *Manager) Stop() {
	m.mu.Lock()
	c := m.cron
	m.mu.Unlock()
	if c != nil {
		<-c.Stop().Done()
	}
}

func (m *Manager) runJob(id int64) {
	j, err := m.Store.Job(id)
	if err != nil || !j.Enabled {
		return
	}
	switch j.Kind {
	case "refresh":
		_, err = m.Refresh(j.PlatformID, &id, "定时:"+j.Name, "cron")
	case "quota":
		_, err = m.ApplyQuota(j.PlatformID, &id, "定时:"+j.Name, "cron", *j.Plan)
	}
	if errors.Is(err, ErrBusy) {
		if rid, e := m.Store.CreateRun(j.PlatformID, &id, j.Kind, "定时:"+j.Name, "cron"); e == nil {
			_ = m.Store.FinishRun(rid, "skipped", 0, 0, 0, ErrBusy.Error())
		}
	} else if err != nil {
		m.Log.Error("job failed to start", zap.Int64("job", id), zap.Error(err))
	}
}

// ---- runs ----

// progress persists a running run's counters; it is a no-op for runs that report only at the end.
type progress func(total, ok, fail int)

type work func(ctx context.Context, c *newapi.Client, report progress) (total, ok int, errs []string, err error)

// launch starts fn in the background under the platform lock and records a run.
func (m *Manager) launch(platformID int64, jobID *int64, kind, title, by string, fn work) (int64, error) {
	p, err := m.Store.Platform(platformID)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	if m.busy[platformID] {
		m.mu.Unlock()
		return 0, ErrBusy
	}
	m.busy[platformID] = true
	m.mu.Unlock()

	runID, err := m.Store.CreateRun(platformID, jobID, kind, title, by)
	if err != nil {
		m.release(platformID)
		return 0, err
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer m.release(platformID)
		total, ok, errs, err := fn(m.ctx, m.Client(p), func(total, ok, fail int) {
			if e := m.Store.Progress(runID, total, ok, fail); e != nil {
				m.Log.Warn("save progress", zap.Error(e))
			}
		})
		status := "ok"
		switch {
		case m.ctx.Err() != nil:
			status = "interrupted"
		case err != nil:
			status = "failed"
			errs = append([]string{err.Error()}, errs...)
		case len(errs) > 0 && ok == 0:
			status = "failed"
		case len(errs) > 0:
			status = "partial"
		}
		if len(errs) > 50 {
			errs = append(errs[:50], fmt.Sprintf("… 另有 %d 条错误", len(errs)-50))
		}
		if e := m.Store.FinishRun(runID, status, total, ok, total-ok, strings.Join(errs, "\n")); e != nil {
			m.Log.Error("save run", zap.Error(e))
		}
	}()
	return runID, nil
}

func (m *Manager) release(id int64) {
	m.mu.Lock()
	delete(m.busy, id)
	m.mu.Unlock()
}

// Refresh recomputes and stores the platform statistics in the background.
func (m *Manager) Refresh(platformID int64, jobID *int64, title, by string) (int64, error) {
	if title == "" {
		title = "刷新统计"
	}
	return m.launch(platformID, jobID, "refresh", title, by, func(ctx context.Context, c *newapi.Client, _ progress) (int, int, []string, error) {
		qpu, err := c.QuotaPerUnit(ctx)
		if err != nil {
			return 0, 0, nil, err
		}
		names, err := c.Groups(ctx)
		if err != nil {
			return 0, 0, nil, fmt.Errorf("读取分组失败(PAT 需要 group:read): %w", err)
		}
		users, err := c.AllUsers(ctx, "")
		if err != nil {
			return 0, 0, nil, err
		}
		st := Stats{At: time.Now().Unix(), QuotaPerUnit: qpu, Users: len(users)}
		by := map[string]*GroupStat{}
		for _, n := range names {
			by[n] = &GroupStat{Name: n}
		}
		for _, u := range users {
			g := by[u.Group]
			if g == nil {
				g = &GroupStat{Name: u.Group}
				by[u.Group] = g
			}
			g.Users++
			g.Quota += u.Quota
			g.Used += u.UsedQuota
			g.Requests += u.RequestCount
			st.Quota += u.Quota
			st.Used += u.UsedQuota
			st.Requests += u.RequestCount
		}
		for _, g := range by {
			st.Groups = append(st.Groups, *g)
		}
		sort.Slice(st.Groups, func(i, j int) bool { return st.Groups[i].Name < st.Groups[j].Name })
		b, _ := json.Marshal(st)
		return 1, 1, nil, m.Store.SaveSnapshot(platformID, b)
	})
}

// ValidatePlan checks a quota plan.
func ValidatePlan(p store.QuotaPlan) error {
	switch p.Mode {
	case "add", "subtract":
		if p.Amount <= 0 {
			return errors.New("增加/扣减的金额必须大于 0")
		}
	case "override":
		if p.Amount < 0 {
			return errors.New("金额不能为负")
		}
	default:
		return errors.New("mode 必须是 add / subtract / override")
	}
	if p.Unit != "usd" && p.Unit != "quota" {
		return errors.New("unit 必须是 usd 或 quota")
	}
	if (p.Group == "") == (len(p.UserIDs) == 0) {
		return errors.New("必须且只能指定 group 或 user_ids 之一")
	}
	return nil
}

// ApplyQuota adjusts balances for the plan's users in the background.
// A user's failure is recorded and the run continues; nothing is retried
// automatically, so a balance is never changed twice by one run.
func (m *Manager) ApplyQuota(platformID int64, jobID *int64, title, by string, plan store.QuotaPlan) (int64, error) {
	if err := ValidatePlan(plan); err != nil {
		return 0, err
	}
	if title == "" {
		title = "调整余额"
	}
	return m.launch(platformID, jobID, "quota", title, by, func(ctx context.Context, c *newapi.Client, report progress) (int, int, []string, error) {
		scale := 1.0
		if plan.Unit == "usd" {
			qpu, err := c.QuotaPerUnit(ctx)
			if err != nil {
				return 0, 0, nil, err
			}
			scale = qpu
		}
		// Check before converting: a float beyond int64 has an implementation-defined result.
		if f := plan.Amount * scale; math.IsNaN(f) || f < 0 || f > maxWalletQuota {
			return 0, 0, nil, errors.New("金额超过 new-api 余额上限")
		}
		value := int64(math.Round(plan.Amount * scale))

		var users []newapi.User
		var errs []string
		if plan.Group != "" {
			var err error
			if users, err = c.AllUsers(ctx, plan.Group); err != nil {
				return 0, 0, nil, err
			}
		} else {
			seen := map[int64]bool{} // a repeated ID would be adjusted twice from the same balance snapshot
			for _, id := range plan.UserIDs {
				if seen[id] {
					continue
				}
				seen[id] = true
				u, err := c.User(ctx, id)
				if err != nil {
					errs = append(errs, fmt.Sprintf("用户 %d: %v", id, err))
					continue
				}
				users = append(users, u)
			}
		}

		ok := 0
		total := len(users) + len(errs)
		report(total, ok, len(errs))
		failed := len(errs)
		for _, u := range users {
			if ctx.Err() != nil {
				break
			}
			// new-api itself lets a subtraction go negative; we refuse, judged on the balance
			// read at the start of the run (a user who spends in between can still go negative).
			switch {
			case plan.Mode == "subtract" && u.Quota < value:
				errs = append(errs, fmt.Sprintf("%s(%d): 余额 %d 不足以扣减 %d", u.Username, u.ID, u.Quota, value))
				failed++
			case plan.Mode == "add" && u.Quota > maxWalletQuota-value:
				errs = append(errs, fmt.Sprintf("%s(%d): 增加后将超过余额上限", u.Username, u.ID))
				failed++
			default:
				if err := c.Adjust(ctx, u.ID, plan.Mode, value); err != nil {
					errs = append(errs, fmt.Sprintf("%s(%d): %v", u.Username, u.ID, err))
					failed++
				} else {
					ok++
				}
			}
			report(total, ok, failed)
		}
		return total, ok, errs, nil
	})
}
