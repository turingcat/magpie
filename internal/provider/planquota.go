package provider

// A plan bought with an API key — Zhipu's GLM Coding Plan (and Z.ai's),
// Kimi Code, OpenCode Go, a Command Code plan, MiniMax's Coding (Token)
// Plan and StepFun's Step Plan —
// has windows of allowance like a subscription's, which the vendor tells
// to the key (StepFun only to a sign-in, stepfun_plan.go): the Usage page
// shows them beside the subscriptions'.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// planQuotaSource is where a provider's key tells its plan's windows.
type planQuotaSource struct {
	url         string
	bearer      bool // Zhipu takes the bare key as the Authorization
	read        func(body []byte) (plan string, ws []QuotaWindow, err error)
	readBalance func(body []byte) (string, error)
	// sure is set when the provider is a plan and not just the vendor: a
	// pay-as-you-go GLM key has no windows to tell, and saying so on a card
	// would only be noise
	sure bool
}

func planQuotaSourceOf(p Provider) (planQuotaSource, bool) {
	if p.Preset == "sub2api" {
		for _, base := range []string{p.Chat, p.Responses, p.Anthropic} {
			if usage, ok := sub2APIUsageURL(base); ok {
				return planQuotaSource{url: usage, bearer: true, read: readSub2APIWindows, readBalance: readSub2APIBalance, sure: true}, true
			}
		}
		return planQuotaSource{}, false
	}
	for _, base := range []string{p.Chat, p.Anthropic, p.Responses} {
		coding := strings.Contains(base, "/api/coding/")
		switch hostOf(base) {
		case "open.bigmodel.cn":
			return planQuotaSource{url: "https://open.bigmodel.cn/api/monitor/usage/quota/limit", read: readZhipuPlan, sure: coding}, true
		case "api.z.ai":
			return planQuotaSource{url: "https://api.z.ai/api/monitor/usage/quota/limit", read: readZhipuPlan, sure: coding}, true
		case "api.kimi.com", "api.kimi.ai":
			if strings.Contains(base, "/coding") {
				return planQuotaSource{url: strings.TrimSuffix(kimiCodeBase(base), "/") + "/usages", bearer: true, read: readKimiCode, sure: true}, true
			}
		case "api.commandcode.ai":
			// a plan's key, from Studio or its sign-in, works on the keyed
			// preset too, and is told the plan's 5-hour and weekly windows;
			// a pay-as-you-go key has none, and no card
			return planQuotaSource{url: "https://api.commandcode.ai/alpha/billing/credits", bearer: true, read: readCommandCodePlan}, true
		case "api.minimaxi.com", "api.minimax.io":
			// a Coding Plan key (sk-cp-…) is told its windows; a
			// pay-as-you-go key isn't, and gets no card (#387)
			return planQuotaSource{url: "https://" + hostOf(base) + "/v1/token_plan/remains", bearer: true, read: readMiniMaxPlan}, true
		case "opencode.ai":
			if u := strings.TrimSuffix(base, "/"); strings.HasSuffix(u, "/zen/go") || strings.Contains(u, "/zen/go/") {
				return planQuotaSource{url: "https://opencode.ai/zen/go/v1/usage", bearer: true, read: readOpenCodeGo, sure: true}, true
			}
		}
	}
	return planQuotaSource{}, false
}

// sub2APIUsageURL derives a self-hosted sub2api usage URL from its configured
// protocol base. The preset gate in planQuotaSourceOf keeps other custom
// OpenAI-compatible providers from being probed.
func sub2APIUsageURL(base string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/usage"
	u.RawPath = ""
	u.RawQuery, u.Fragment = "", ""
	return u.String(), true
}

// readZhipuPlan reads
//
//	{"success":true,"data":{"level":"pro","limits":[
//	  {"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":12,"nextResetTime":1758800000000},
//	  {"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":40,"nextResetTime":…},
//	  {"type":"TIME_LIMIT","unit":5,"number":1,"percentage":3,"nextResetTime":…}]}}
//
// unit 3 is hours and 6 weeks (number 1 or 7 have both been seen for the
// week); TIME_LIMIT is the month's MCP tool calls, which don't stop the
// models.
func readZhipuPlan(b []byte) (string, []QuotaWindow, error) {
	var r struct {
		Success *bool  `json:"success"`
		Msg     string `json:"msg"`
		Data    *struct {
			Level  string `json:"level"`
			Limits []struct {
				Type          string   `json:"type"`
				Unit          int      `json:"unit"`
				Number        int      `json:"number"`
				Percentage    *float64 `json:"percentage"`
				NextResetTime int64    `json:"nextResetTime"`
			} `json:"limits"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "", nil, err
	}
	if r.Success != nil && !*r.Success || r.Data == nil {
		if r.Msg != "" {
			return "", nil, fmt.Errorf("%s", r.Msg)
		}
		return "", nil, fmt.Errorf("no plan in the reply")
	}
	out := []QuotaWindow{}
	for _, l := range r.Data.Limits {
		w := QuotaWindow{}
		if l.Percentage != nil {
			w.Used = *l.Percentage
		}
		if l.NextResetTime > 0 {
			t := time.UnixMilli(l.NextResetTime)
			w.ResetsAt = &t
		}
		switch {
		case strings.EqualFold(l.Type, "TIME_LIMIT"):
			w.Name, w.Aside = "MCP · Month", true
		case l.Unit == 3:
			n := max(l.Number, 1)
			if l.Number <= 0 { // a team's five hours come with no number
				n = 5
			}
			w.Name, w.Span = fmt.Sprintf("%d hours", n), time.Duration(n)*time.Hour
		case l.Unit == 6:
			w.Name, w.Span = "7 days", 7*24*time.Hour
		default:
			w.Name = "Allowance"
		}
		out = append(out, w)
	}
	return r.Data.Level, out, nil
}

// readCommandCodePlan reads a Command Code key's /alpha/billing/credits
// (see cmdCredits) for its plan's 5-hour and weekly windows. The dollars
// left are the key's balance card's (readCommandCode), so not a window
// here as on the signed-in subscription's card.
func readCommandCodePlan(b []byte) (string, []QuotaWindow, error) {
	var c cmdCredits
	if err := json.Unmarshal(b, &c); err != nil {
		return "", nil, err
	}
	return cmdPlanName(c.Credits.PlanID), cmdWindows(c), nil
}

// readOpenCodeGo reads
//
//	{"usage":{"rolling":{"status":"ok","percent":37,"resetsAt":"2026-08-26T14:12:03.000Z"},
//	          "weekly":{…},"monthly":{"status":"rate-limited","percent":100,…}}}
//
// A window at 0% gives now plus its length as resetsAt, a time nothing
// happens at, so it is left out.
func readOpenCodeGo(b []byte) (string, []QuotaWindow, error) {
	type window struct {
		Percent  *float64 `json:"percent"`
		ResetsAt string   `json:"resetsAt"`
	}
	var r struct {
		Usage map[string]window `json:"usage"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "", nil, err
	}
	out := []QuotaWindow{}
	for _, x := range []struct {
		key, name string
		span      time.Duration
	}{{"rolling", "5 hours", 5 * time.Hour}, {"weekly", "7 days", 7 * 24 * time.Hour}, {"monthly", "Month", 0}} {
		w, ok := r.Usage[x.key]
		if !ok || w.Percent == nil {
			continue
		}
		q := QuotaWindow{Name: x.name, Used: *w.Percent, Span: x.span}
		if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil && *w.Percent > 0 {
			q.ResetsAt = &t
		}
		out = append(out, q)
	}
	if len(out) == 0 {
		return "", nil, fmt.Errorf("no usage in the reply")
	}
	return "", out, nil
}

// readSub2APIWindows extracts the allowance windows from sub2api's
// unrestricted /usage response.
func readSub2APIWindows(b []byte) (string, []QuotaWindow, error) {
	plan, ws, _, err := readSub2APIPlan(b)
	return plan, ws, err
}

// readSub2APIBalance extracts the account balance from sub2api's /usage reply.
func readSub2APIBalance(b []byte) (string, error) {
	_, _, balance, err := readSub2APIPlan(b)
	return balance, err
}

func readSub2APIPlan(b []byte) (string, []QuotaWindow, string, error) {
	var r struct {
		Plan   string   `json:"planName"`
		Remain *float64 `json:"remaining"`
		Unit   string   `json:"unit"`
		Sub    *struct {
			DailyUsage   float64  `json:"daily_usage_usd"`
			DailyLimit   *float64 `json:"daily_limit_usd"`
			WeeklyUsage  float64  `json:"weekly_usage_usd"`
			WeeklyLimit  *float64 `json:"weekly_limit_usd"`
			MonthlyUsage float64  `json:"monthly_usage_usd"`
			MonthlyLimit *float64 `json:"monthly_limit_usd"`
			WeeklyStart  string   `json:"weekly_window_start"`
		} `json:"subscription"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "", nil, "", err
	}
	balance := ""
	if r.Remain != nil {
		if strings.EqualFold(strings.TrimSpace(r.Unit), "USD") || strings.TrimSpace(r.Unit) == "" {
			balance = fmt.Sprintf("$%.2f", *r.Remain)
		} else {
			balance = fmt.Sprintf("%.2f %s", *r.Remain, strings.TrimSpace(r.Unit))
		}
	}
	var ws []QuotaWindow
	if r.Sub != nil {
		add := func(name string, used float64, limit *float64, span time.Duration) {
			if limit == nil || *limit <= 0 {
				return
			}
			pct := used / *limit * 100
			pct = max(0, min(100, pct))
			ws = append(ws, QuotaWindow{Name: name, Used: pct, Span: span})
		}
		add("24 hours", r.Sub.DailyUsage, r.Sub.DailyLimit, 24*time.Hour)
		add("7 days", r.Sub.WeeklyUsage, r.Sub.WeeklyLimit, 7*24*time.Hour)
		add("30 days", r.Sub.MonthlyUsage, r.Sub.MonthlyLimit, 30*24*time.Hour)
		if len(ws) > 0 && r.Sub.WeeklyStart != "" {
			if t, err := time.Parse(time.RFC3339Nano, r.Sub.WeeklyStart); err == nil {
				for i := range ws {
					if ws[i].Name == "7 days" {
						reset := t.Add(7 * 24 * time.Hour)
						ws[i].ResetsAt = &reset
					}
				}
			}
		}
	}
	if balance == "" && len(ws) == 0 {
		return "", nil, "", fmt.Errorf("no balance or usage in the reply")
	}
	return r.Plan, ws, balance, nil
}

// readMiniMaxPlan reads MiniMax's /v1/token_plan/remains (#387):
//
//	{"model_remains":[{"model_name":"general",
//	   "start_time":…,"end_time":…,"current_interval_remaining_percent":100,
//	   "current_interval_status":1,"current_interval_total_count":0,
//	   "weekly_start_time":…,"weekly_end_time":…,"current_weekly_remaining_percent":100,
//	   "current_weekly_status":1,"current_weekly_total_count":0},
//	  {"model_name":"video",…}],
//	 "base_resp":{"status_code":0,"status_msg":"success"}}
//
// Each bucket is a quota with a rolling interval and a week, told as what
// remains. "general" is what the models draw on; another (video, image…)
// is metered apart and doesn't stop them. A bucket the plan doesn't have
// comes as both windows unlimited (status 3) with no totals, and is left
// out; status 2 is used up, whatever the percentage says. MiniMax answers
// 200 to a key it refuses, with the reason in base_resp.
func readMiniMaxPlan(b []byte) (string, []QuotaWindow, error) {
	type bucket struct {
		Model      string   `json:"model_name"`
		Start      int64    `json:"start_time"`
		End        int64    `json:"end_time"`
		Left       *float64 `json:"current_interval_remaining_percent"`
		Status     int      `json:"current_interval_status"`
		Total      *float64 `json:"current_interval_total_count"`
		WeekStart  int64    `json:"weekly_start_time"`
		WeekEnd    int64    `json:"weekly_end_time"`
		WeekLeft   *float64 `json:"current_weekly_remaining_percent"`
		WeekStatus int      `json:"current_weekly_status"`
		WeekTotal  *float64 `json:"current_weekly_total_count"`
	}
	var r struct {
		Remains []bucket `json:"model_remains"`
		Base    *struct {
			Code int    `json:"status_code"`
			Msg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "", nil, err
	}
	if r.Base == nil {
		return "", nil, fmt.Errorf("no plan in the reply")
	}
	if r.Base.Code != 0 {
		if r.Base.Msg != "" {
			return "", nil, fmt.Errorf("%s", r.Base.Msg)
		}
		return "", nil, fmt.Errorf("MiniMax said %d", r.Base.Code)
	}
	at := func(n int64) time.Time {
		if n < 1e12 { // seconds
			return time.Unix(n, 0)
		}
		return time.UnixMilli(n)
	}
	zero := func(f *float64) bool { return f != nil && *f == 0 }
	out := []QuotaWindow{}
	for _, k := range r.Remains {
		name := strings.TrimSpace(k.Model)
		if name == "" || k.Status == 3 && k.WeekStatus == 3 && zero(k.Total) && zero(k.WeekTotal) {
			continue // not in the plan
		}
		general := strings.EqualFold(name, "general")
		for _, x := range []struct {
			left       *float64
			status     int
			start, end int64
			week       bool
		}{{k.Left, k.Status, k.Start, k.End, false}, {k.WeekLeft, k.WeekStatus, k.WeekStart, k.WeekEnd, true}} {
			if x.status == 3 || x.left == nil && x.status != 2 {
				continue // unlimited, or nothing told
			}
			w := QuotaWindow{Used: 100}
			if x.status != 2 {
				w.Used = max(0, min(100, 100-*x.left))
			}
			if x.start > 0 && x.end > x.start {
				w.Span = at(x.end).Sub(at(x.start))
			} else if x.week {
				w.Span = 7 * 24 * time.Hour
			}
			if x.end > 0 {
				t := at(x.end)
				w.ResetsAt = &t
			}
			switch span := w.Span; {
			case x.week:
				w.Name = "7 days"
			case span > 24*time.Hour && span%(24*time.Hour) == 0:
				w.Name = fmt.Sprintf("%d days", span/(24*time.Hour))
			case span >= time.Hour && span%time.Hour == 0:
				w.Name = fmt.Sprintf("%d hours", span/time.Hour)
			case span > 0:
				w.Name = fmt.Sprintf("%d minutes", span/time.Minute)
			default:
				w.Name = "Allowance"
			}
			if !general {
				w.Name = strings.ToUpper(name[:1]) + name[1:] + " · " + w.Name
				w.Aside = true
			}
			out = append(out, w)
		}
	}
	return "", out, nil
}

// kimiCodeBase is Kimi Code's OpenAI endpoint, /coding/v1, for either of
// the provider's (its Anthropic one is /coding).
func kimiCodeBase(base string) string {
	u := strings.TrimSuffix(base, "/")
	if strings.HasSuffix(u, "/coding") {
		return u + "/v1"
	}
	return u
}

// readKimiCode reads Kimi Code's /usages, as kimi-cli's /usage does:
//
//	{"usage":{"limit":"100","used":"12","resetTime":"2026-09-30T05:24:18.44Z"},
//	 "limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},
//	   "detail":{"limit":"100","remaining":"88","resetTime":"…"}}]}
//
// usage is the week's allowance, each of limits a shorter window; the
// numbers come as strings or numbers, used or what remains.
func readKimiCode(b []byte) (string, []QuotaWindow, error) {
	type row map[string]any
	var r struct {
		Usage  row `json:"usage"`
		Limits []struct {
			Window row `json:"window"`
			Detail row `json:"detail"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return "", nil, err
	}
	num := func(v any) (float64, bool) {
		switch x := v.(type) {
		case float64:
			return x, true
		case string:
			f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
			return f, err == nil
		}
		return 0, false
	}
	window := func(d row, name string, span time.Duration) (QuotaWindow, bool) {
		limit, ok := num(d["limit"])
		if !ok || limit <= 0 {
			return QuotaWindow{}, false
		}
		used, ok := num(d["used"])
		if !ok {
			left, ok := num(d["remaining"])
			if !ok {
				return QuotaWindow{}, false
			}
			used = limit - left
		}
		w := QuotaWindow{Name: name, Span: span, Used: max(0, min(100, used/limit*100))}
		for _, k := range []string{"resetTime", "resetAt", "reset_at", "reset_time"} {
			if s, _ := d[k].(string); s != "" {
				if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
					w.ResetsAt = &t
					break
				}
			}
		}
		return w, true
	}
	out := []QuotaWindow{}
	for _, l := range r.Limits {
		d := l.Detail
		if d == nil {
			d = l.Window
		}
		n, _ := num(l.Window["duration"])
		unit, _ := l.Window["timeUnit"].(string)
		var span time.Duration
		switch {
		case strings.Contains(unit, "MINUTE"):
			span = time.Duration(n) * time.Minute
		case strings.Contains(unit, "HOUR"):
			span = time.Duration(n) * time.Hour
		case strings.Contains(unit, "DAY"):
			span = time.Duration(n) * 24 * time.Hour
		}
		name := "Allowance"
		switch {
		case span > 24*time.Hour && span%(24*time.Hour) == 0:
			name = fmt.Sprintf("%d days", span/(24*time.Hour))
		case span >= time.Hour && span%time.Hour == 0:
			name = fmt.Sprintf("%d hours", span/time.Hour)
		case span > 0:
			name = fmt.Sprintf("%d minutes", span/time.Minute)
		}
		if w, ok := window(d, name, span); ok {
			out = append(out, w)
		}
	}
	if w, ok := window(r.Usage, "7 days", 7*24*time.Hour); ok {
		out = append(out, w)
	}
	if len(out) == 0 {
		return "", nil, fmt.Errorf("no usage in the reply")
	}
	return "", out, nil
}

// planWindows asks the vendor for the plan key is on and its windows.
func planWindows(ctx context.Context, src planQuotaSource, key string) (plan string, ws []QuotaWindow, balance string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.url, nil)
	if err != nil {
		return "", nil, "", err
	}
	if src.bearer {
		req.Header.Set("Authorization", "Bearer "+key)
	} else {
		req.Header.Set("Authorization", key)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, "", err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	switch {
	case res.StatusCode == http.StatusForbidden && strings.Contains(src.url, "opencode.ai"):
		return "", nil, "", fmt.Errorf("this key has no OpenCode Go subscription")
	case res.StatusCode >= 300:
		return "", nil, "", fmt.Errorf("%s", res.Status)
	}
	plan, ws, err = src.read(b)
	if err != nil {
		return "", nil, "", err
	}
	if src.readBalance != nil {
		balance, err = src.readBalance(b)
	}
	return plan, ws, balance, err
}

var planQuotaCache struct {
	sync.Mutex
	at   time.Time
	data []SubscriptionQuota
}

// PlanQuotas is the windows of every plan magpie has a key for, each key
// on a card of its own when a provider has several. What was asked less
// than a minute ago is not asked again.
func PlanQuotas(ctx context.Context) []SubscriptionQuota {
	c := &planQuotaCache
	c.Lock()
	if c.data != nil && time.Since(c.at) < time.Minute {
		defer c.Unlock()
		return c.data
	}
	c.Unlock()
	type job struct {
		p    Provider
		src  planQuotaSource
		key  string
		user string
	}
	var jobs []job
	for _, p := range All() {
		if p.Hidden || p.Off || p.Account != nil || p.Key == "" {
			continue
		}
		src, ok := planQuotaSourceOf(p)
		if !ok {
			continue
		}
		keys := []string{p.Key}
		names := []string{p.KeyName}
		for _, k := range p.Keys {
			if !k.Off && k.Key != "" && k.Key != p.Key {
				keys, names = append(keys, k.Key), append(names, k.Name)
			}
		}
		for i, k := range keys {
			user := ""
			if len(keys) > 1 {
				if user = names[i]; user == "" {
					user = Mask(k)
				}
			}
			jobs = append(jobs, job{p, src, k, user})
		}
	}
	got := make([]*SubscriptionQuota, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := SubscriptionQuota{Provider: j.p.ID, Name: j.p.Name, Icon: j.p.Icon, User: j.user, Windows: []QuotaWindow{}}
			plan, ws, balance, err := planWindows(j.p.Via(ctx), j.src, j.key)
			team := false
			if zhipu := strings.HasSuffix(j.src.url, "/api/monitor/usage/quota/limit"); zhipu && (err != nil || len(ws) == 0) {
				// no plan of the key's own: a team's key, whose windows are
				// asked with type=2 (zcode_team.go)
				if tplan, tws, terr := zhipuKeyTeamWindows(j.p.Via(ctx), j.src.url, j.key, j.p.ZhipuTeam); terr == nil && len(tws) > 0 {
					plan, ws, balance, err, team = tplan, tws, "", nil, true
				}
			}
			// a vendor failing a while (Command Code answers billing/credits
			// 503 at times) shows what was last read, as a subscription's
			// card does, rather than no card or "Usage unavailable"
			tag := keyTag("plan", j.key)
			switch {
			case err == nil && len(ws) == 0 && balance == "":
				return // a key with no plan
			case err != nil && !j.src.sure:
				// no plan, unless one was read before
				q.Error = err.Error()
				if q = keepLast(q, tag); q.AsOf != nil {
					got[i] = &q
				}
				return
			case err != nil:
				q.Error = err.Error()
			default:
				q.Plan, q.Windows, q.Balance = plan, ws, balance
				if strings.HasSuffix(j.src.url, "/api/monitor/usage/quota/limit") && !team { // Zhipu, Z.ai
					q.Until, q.Renew = zhipuTerm(ctx, zcodeRoot(j.src.url), j.key)
				}
			}
			q = keepLast(q, tag)
			got[i] = &q
		}()
	}
	stepfun := make(chan []SubscriptionQuota, 1)
	go func() { stepfun <- stepPlanQuotas(ctx) }()
	wg.Wait()
	out := []SubscriptionQuota{}
	for _, q := range got {
		if q != nil {
			out = append(out, *q)
		}
	}
	out = append(out, <-stepfun...)
	if ctx.Err() == nil {
		c.Lock()
		c.at, c.data = time.Now(), out
		c.Unlock()
	}
	return out
}

// ZhipuTeam is the team a Zhipu or Z.ai key's GLM Coding Plan belongs to:
// its organization and project IDs, as the BigModel console shows them.
type ZhipuTeam struct {
	Org     string `json:"org,omitempty"`
	Project string `json:"project,omitempty"`
}

// normal is t as it is kept: trimmed, nil when neither is given.
func (t *ZhipuTeam) normal() *ZhipuTeam {
	if t == nil {
		return nil
	}
	n := ZhipuTeam{strings.TrimSpace(t.Org), strings.TrimSpace(t.Project)}
	if n.Org == "" && n.Project == "" {
		return nil
	}
	return &n
}

// TakesZhipuTeam says p is a key of Zhipu's or Z.ai's, whose editor then
// offers the team's organization and project (#236).
func TakesZhipuTeam(p Provider) bool {
	if p.Account != nil {
		return false
	}
	src, ok := planQuotaSourceOf(p)
	return ok && strings.HasSuffix(src.url, "/api/monitor/usage/quota/limit")
}

// zhipuKeyTeamWindows is a GLM key's windows on a team's GLM Coding Plan:
// the quota asked with type=2 where ZCode asks it (bigmodel.cn, api.z.ai),
// with the team's organization and project in the headers: those the user
// gave the provider, else those of a ZCode account's team key magpie holds
// (a key pasted alone is asked without them, which is a guess: ZCode
// always sends them).
func zhipuKeyTeamWindows(ctx context.Context, quotaURL, key string, team *ZhipuTeam) (plan string, ws []QuotaWindow, err error) {
	// the key's own host first (open.bigmodel.cn, as CC Switch asks it),
	// then where ZCode asks it
	for _, root := range zhipuTeamRoots(quotaURL) {
		if plan, ws, err = zhipuKeyTeamAt(ctx, root, quotaURL, key, team); err == nil && len(ws) > 0 {
			return
		}
	}
	return
}

// zhipuTeamRoots are the hosts a team's quota is asked at, each once.
func zhipuTeamRoots(quotaURL string) []string {
	own := strings.TrimSuffix(quotaURL, "/api/monitor/usage/quota/limit")
	if biz := zcodeBizRoot(quotaURL); biz != own {
		return []string{own, biz}
	}
	return []string{own}
}

func zhipuKeyTeamAt(ctx context.Context, root, quotaURL, key string, team *ZhipuTeam) (string, []QuotaWindow, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/api/monitor/usage/quota/limit?type=2", nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en")
	org, project := zhipuTeamOf(key)
	if team != nil && team.Org != "" && team.Project != "" {
		org, project = team.Org, team.Project
	}
	if org != "" {
		for k, v := range zcodeTeamHeaders(quotaURL, org, project) {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		return "", nil, fmt.Errorf("%s", res.Status)
	}
	_, ws, err := readZhipuPlan(b)
	return "GLM Coding Team", ws, err
}
