package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nerdneilsfield/newapi-multi-website-manager/internal/buildinfo"
)

// The MCP endpoint is the official SDK's streamable HTTP handler. Every tool is a call to the
// REST API made in-process with the caller's own bearer token, so grants, validation and the
// audit log are the REST ones and live in one place.

type platformArg struct {
	PlatformID int64 `json:"platform_id" jsonschema:"平台 ID，来自 list_platforms"`
}

type jobArg struct {
	PlatformID int64 `json:"platform_id" jsonschema:"平台 ID，来自 list_platforms"`
	JobID      int64 `json:"job_id" jsonschema:"任务 ID，来自 list_jobs"`
}

type usersArg struct {
	PlatformID int64  `json:"platform_id" jsonschema:"平台 ID，来自 list_platforms"`
	Group      string `json:"group,omitempty" jsonschema:"只看该分组"`
	Keyword    string `json:"keyword,omitempty" jsonschema:"按用户名或 ID 搜索"`
	Page       int    `json:"page,omitempty" jsonschema:"页码，从 1 开始"`
}

type planArg struct {
	Mode    string  `json:"mode" jsonschema:"add 增加、subtract 扣减、override 设为"`
	Amount  float64 `json:"amount" jsonschema:"金额，不小于 0"`
	Unit    string  `json:"unit" jsonschema:"usd 为美元，quota 为 new-api 原始额度"`
	Group   string  `json:"group,omitempty" jsonschema:"分组名；与 user_ids 二选一"`
	UserIDs []int64 `json:"user_ids,omitempty" jsonschema:"用户 ID 列表；与 group 二选一"`
}

type quotaArg struct {
	PlatformID int64 `json:"platform_id" jsonschema:"平台 ID，来自 list_platforms"`
	planArg
}

type jobBody struct {
	Name    string   `json:"name" jsonschema:"任务名称"`
	Kind    string   `json:"kind" jsonschema:"refresh 刷新统计，usage 采集用量，quota 调整余额"`
	Spec    string   `json:"spec" jsonschema:"5 段 cron，或 @every 1h"`
	Enabled bool     `json:"enabled" jsonschema:"是否启用；false 时任务只保存，不会按时执行"`
	Plan    *planArg `json:"plan,omitempty" jsonschema:"kind 为 quota 时必填，字段同 adjust_quota"`
}

type createJobArg struct {
	PlatformID int64 `json:"platform_id" jsonschema:"平台 ID，来自 list_platforms"`
	jobBody
}

type updateJobArg struct {
	PlatformID int64 `json:"platform_id" jsonschema:"平台 ID，来自 list_platforms"`
	JobID      int64 `json:"job_id" jsonschema:"任务 ID，来自 list_jobs"`
	jobBody
}

type auditArg struct {
	Before int64 `json:"before,omitempty" jsonschema:"上一页最后一条的 id，返回比它更早的记录"`
}

// usageGuide is the get_usage description. napm computes nothing here: it returns stored rows and tells the
// model what to work out from them, the same set the Usage page shows. Keep its thresholds and windows in step
// with analyze() in index.html (between `// <analysis>` and `// </analysis>`).
const usageGuide = `读取已采集的用量行，不做任何分析；分析由你完成。
返回 {state, from, to, step, models, users, user_groups}。user_groups 是 {username: 分组名}，来自上次刷新统计，按分组汇总时用它把用户归到分组。models 每行是 {t, model, count, quota, tokens}，users 每行是 {t, username, count, quota, tokens}。t 是桶的起点(Unix 秒)，count 是请求数，quota 是 new-api 原始额度，除以 state.quota_per_unit 得金额。state 为 null 表示还没采集过，先用 collect_usage 并用 list_runs 等它完成；state.collected_to 之后没有数据。

回答「花得怎么样、谁在用、有没有异常」时，先取 step=day，tz 用用户所在时区的分钟数(东八区是 480)。from 必须是用户时区里今天零点往前 2N 天的那个零点(不是「当前时间减 2N 天」，否则最早一天只有半天数据，上期会被低估)，to 取当前时间；这样最近 N 个完整日是今天零点往前 N 天到今天零点，上期是再往前 N 天。今天还没过完，不要把它算进对比。再按下面的方法算：
1. 对比：把最近 N 个完整日的 quota 之和，与再前 N 日的和比较，给出涨跌百分比；上期为 0 时只说明没有可比数据，不要除零。
2. 月度推算：最近 7 个完整日的日均乘 30。
3. 异常日：对最近 N 个完整日的 quota 取中位数 m 和各日与 m 的绝对偏差的中位数 d，离散度 s = max(1.4826*d, 0.25*m)；日值 > m+3*s 的日子算异常。m 为 0 或不足 7 天时不判断。
4. 集中度：按 model 或 username 汇总 quota，算占比；单个用户 >= 30% 或单个模型 >= 50% 值得点出，并给前 3 名合计占比。
5. 涨跌：按 username 分别汇总本期与上期 quota，找涨幅和跌幅最大的，只在变化量超过上期总额 10% 时才提；统计本期出现而上期没有、上期有而本期没有的用户数。
6. 一天内的分布：只在需要时再取 step=hour，from 取今天本地零点往前 min(N,13) 天，to 取今天本地零点，按本地小时累加 count 得到 24 个值；至少 12 个小时非零才判断，找总和最小的连续 4 小时(可绕过午夜；适合停机或批量调额)和最忙的一小时。
7. 周末：最近 N 个完整日里，周六日至少 2 天、工作日至少 5 天时，算周末日均与工作日日均之比；低于 0.75 或高于 1.33 才提。
8. 单个用户或分组：把这个用户(或分组里所有用户)的 users 行单独汇总，按上面 1、2、3、6、7 的方法算，再给出它占全站 quota 的比例和在排行中的名次。
结论里每一条都带数字和对应的时间范围；数据不足以支持的条目不要写。`

type usageArg struct {
	PlatformID int64  `json:"platform_id" jsonschema:"平台 ID，来自 list_platforms"`
	From       int64  `json:"from,omitempty" jsonschema:"起点，Unix 秒；默认 30 天前"`
	To         int64  `json:"to,omitempty" jsonschema:"终点(不含)，Unix 秒；默认现在"`
	Step       string `json:"step,omitempty" jsonschema:"day 按天(默认)或 hour 按小时(最多 14 天)"`
	TZ         int    `json:"tz,omitempty" jsonschema:"时区，东区为正的分钟数，决定「天」的边界；默认 0 即 UTC"`
}

type none struct{}

// mcpHandler serves /mcp. Only a bearer token is accepted, never the session cookie, so a web
// page the admin has open cannot be made to drive it.
func (s *Server) mcpHandler(rest http.Handler) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "napm", Version: buildinfo.Version}, nil)
	call := func(ctx context.Context, method, path string, q url.Values, body any) (*mcp.CallToolResult, any, error) {
		var buf bytes.Buffer
		if body != nil {
			if err := json.NewEncoder(&buf).Encode(body); err != nil {
				return nil, nil, err
			}
		}
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, method, path, &buf)
		if err != nil {
			return nil, nil, err
		}
		ti := auth.TokenInfoFromContext(ctx)
		if ti == nil {
			return nil, nil, auth.ErrInvalidToken // unreachable behind RequireBearerToken
		}
		req.Header.Set("Authorization", "Bearer "+ti.Extra["token"].(string))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		rest.ServeHTTP(rec, req)
		return &mcp.CallToolResult{
			IsError: rec.Code >= 400,
			Content: []mcp.Content{&mcp.TextContent{Text: rec.Body.String()}},
		}, nil, nil
	}
	pid := func(id int64) string { return "/api/platforms/" + strconv.FormatInt(id, 10) }
	tool := func(name, desc string) *mcp.Tool { return &mcp.Tool{Name: name, Description: desc} }

	mcp.AddTool(srv, tool("list_platforms", "列出当前管理员可管理的平台。"),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ none) (*mcp.CallToolResult, any, error) {
			return call(ctx, "GET", "/api/platforms", nil, nil)
		})
	mcp.AddTool(srv, tool("get_stats", "读取平台最近一次刷新得到的统计：用户数、分组、总余额、已用额度，以及最近一次执行。数据可能不是最新的，需要时先 refresh_stats。"),
		func(ctx context.Context, _ *mcp.CallToolRequest, a platformArg) (*mcp.CallToolResult, any, error) {
			return call(ctx, "GET", pid(a.PlatformID)+"/stats", nil, nil)
		})
	mcp.AddTool(srv, tool("refresh_stats", "开始刷新平台统计。立即返回 run_id，刷新在后台进行，用 list_runs 查看进度。"),
		func(ctx context.Context, _ *mcp.CallToolRequest, a platformArg) (*mcp.CallToolResult, any, error) {
			return call(ctx, "POST", pid(a.PlatformID)+"/refresh", nil, nil)
		})
	mcp.AddTool(srv, tool("list_users", "分页列出平台上的用户（实时请求 new-api）。"),
		func(ctx context.Context, _ *mcp.CallToolRequest, a usersArg) (*mcp.CallToolResult, any, error) {
			q := url.Values{}
			if a.Group != "" {
				q.Set("group", a.Group)
			}
			if a.Keyword != "" {
				q.Set("keyword", a.Keyword)
			}
			if a.Page > 0 {
				q.Set("page", strconv.Itoa(a.Page))
			}
			return call(ctx, "GET", pid(a.PlatformID)+"/users", q, nil)
		})
	mcp.AddTool(srv, tool("adjust_quota", "修改某几个用户或整个分组的余额。会真实改动 new-api 上的余额，add 和 subtract 不是幂等的，重复调用会重复生效；需要幂等时用 override。立即返回 run_id，用 list_runs 查看结果。"),
		func(ctx context.Context, _ *mcp.CallToolRequest, a quotaArg) (*mcp.CallToolResult, any, error) {
			return call(ctx, "POST", pid(a.PlatformID)+"/quota", nil, a.planArg)
		})
	mcp.AddTool(srv, tool("get_usage", usageGuide),
		func(ctx context.Context, _ *mcp.CallToolRequest, a usageArg) (*mcp.CallToolResult, any, error) {
			q := url.Values{}
			for k, v := range map[string]int64{"from": a.From, "to": a.To, "tz": int64(a.TZ)} {
				if v != 0 {
					q.Set(k, strconv.FormatInt(v, 10))
				}
			}
			if a.Step != "" {
				q.Set("step", a.Step)
			}
			return call(ctx, "GET", pid(a.PlatformID)+"/usage", q, nil)
		})
	mcp.AddTool(srv, tool("collect_usage", "开始采集平台用量。立即返回 run_id，在后台增量拉取，用 list_runs 查看进度。需要平台令牌有 log:read 权限。"),
		func(ctx context.Context, _ *mcp.CallToolRequest, a platformArg) (*mcp.CallToolResult, any, error) {
			return call(ctx, "POST", pid(a.PlatformID)+"/usage/collect", nil, nil)
		})
	mcp.AddTool(srv, tool("list_runs", "列出平台最近 50 次执行（刷新、调额），新的在前，含状态、成功数、失败数和错误。"),
		func(ctx context.Context, _ *mcp.CallToolRequest, a platformArg) (*mcp.CallToolResult, any, error) {
			return call(ctx, "GET", pid(a.PlatformID)+"/runs", nil, nil)
		})
	mcp.AddTool(srv, tool("list_jobs", "列出平台的定时任务。"),
		func(ctx context.Context, _ *mcp.CallToolRequest, a platformArg) (*mcp.CallToolResult, any, error) {
			return call(ctx, "GET", pid(a.PlatformID)+"/jobs", nil, nil)
		})
	mcp.AddTool(srv, tool("create_job", "创建定时任务。"),
		func(ctx context.Context, _ *mcp.CallToolRequest, a createJobArg) (*mcp.CallToolResult, any, error) {
			return call(ctx, "POST", pid(a.PlatformID)+"/jobs", nil, a.jobBody)
		})
	mcp.AddTool(srv, tool("update_job", "修改定时任务。整体替换，未给出的字段按默认值处理，所以要带上完整内容。"),
		func(ctx context.Context, _ *mcp.CallToolRequest, a updateJobArg) (*mcp.CallToolResult, any, error) {
			return call(ctx, "PUT", pid(a.PlatformID)+"/jobs/"+strconv.FormatInt(a.JobID, 10), nil, a.jobBody)
		})
	mcp.AddTool(srv, tool("delete_job", "删除定时任务。"),
		func(ctx context.Context, _ *mcp.CallToolRequest, a jobArg) (*mcp.CallToolResult, any, error) {
			return call(ctx, "DELETE", pid(a.PlatformID)+"/jobs/"+strconv.FormatInt(a.JobID, 10), nil, nil)
		})
	mcp.AddTool(srv, tool("list_audit", "读取操作记录，新的在前，每次 200 条。root 看到全部管理员的，其他管理员只看到自己的。"),
		func(ctx context.Context, _ *mcp.CallToolRequest, a auditArg) (*mcp.CallToolResult, any, error) {
			q := url.Values{}
			if a.Before > 0 {
				q.Set("before", strconv.FormatInt(a.Before, 10))
			}
			return call(ctx, "GET", "/api/audit", q, nil)
		})

	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		Stateless:    true, // every call stands alone and carries its own token
		JSONResponse: true,
	})
	return auth.RequireBearerToken(s.verifyToken, &auth.RequireBearerTokenOptions{
		AllowMissingExpiration: true, // napm tokens live until they are deleted
	})(h)
}

// verifyToken is the SDK's TokenVerifier for napm API tokens.
func (s *Server) verifyToken(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	a, ok := s.byToken("Bearer " + token)
	if !ok {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{UserID: a.Username, Extra: map[string]any{"token": token}}, nil
}
