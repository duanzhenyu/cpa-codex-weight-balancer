package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/balancer"
	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/controller"
	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/pluginconf"
)

const (
	pluginName    = "cpa-codex-weight-balancer"
	pluginVersion = "0.7.2"
	// The host rejects a registration whose Author or GitHubRepository is
	// blank, regardless of which capabilities it declares.
	pluginAuthor = "duanzhenyu"
	pluginRepo   = "https://github.com/duanzhenyu/cpa-codex-weight-balancer"
	statusPath   = "/status"
	refreshPath  = "/refresh"
)

var (
	mu      sync.Mutex
	current *worker
)

// worker owns one configuration's ticker and controller.
type worker struct {
	cfg  pluginconf.Resolved
	ctl  *controller.Controller
	stop chan struct{}
	done chan struct{}
	// wake carries rejections from the host's usage queue to the loop. One
	// signal is enough: the controller holds the rejections themselves.
	wake chan struct{}

	resultMu sync.Mutex
	lastRun  time.Time
	lastErr  string
	lastRep  balancer.Report
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

type capabilities struct {
	ManagementAPI bool `json:"management_api"`
	// UsagePlugin subscribes to the host's usage records, which carry the
	// upstream response headers. The host dispatches them from its own queue,
	// so this hook is not on the request path.
	UsagePlugin bool `json:"usage_plugin"`
}

type managementRegistration struct {
	Routes []managementRoute `json:"routes,omitempty"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description"`
}

type managementRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Query   url.Values
	Body    []byte
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := reconfigure(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		stopRuntime()
		return okEnvelope(map[string]any{})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration{Routes: []managementRoute{
			{Method: http.MethodGet, Path: pluginName + statusPath, Description: "Per-credential weight balancer state."},
			{Method: http.MethodPost, Path: pluginName + refreshPath, Description: "Run one probe-and-reweight cycle immediately."},
		}})
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepo,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "interval", Type: pluginapi.ConfigFieldTypeString, Description: "重新定权周期，例如 15m。每轮用请求顺带上报的额度重算权重并写回，没有上报的号才主动探测。每次权重变动都会清空宿主全队列的路由累加器，务必保持在几十分钟量级。最小 1m。"},
				{Name: "endpoint", Type: pluginapi.ConfigFieldTypeString, Description: "额度探测地址。必须是 https —— 每次探测都带着凭据的 access_token。留空则用默认的 Codex usage 端点。"},
				{Name: "quota_capacity", Type: pluginapi.ConfigFieldTypeNumber, Description: "套餐总量，把 used_percent 与观测消耗换算成绝对单位。它是全队伍共用的一个值，不能让不同套餐变得可比，也不改变路由比例。"},
				{Name: "alpha", Type: pluginapi.ConfigFieldTypeNumber, Description: "紧迫度指数。1 表示按周窗口的 deadline 匀速配速；0 表示只看剩余额度、不管刷新时间；大于 1 则更偏向快要刷新的账号。"},
				{Name: "t_floor", Type: pluginapi.ConfigFieldTypeString, Description: "距刷新时间的下限，例如 15m。窗口即将关闭时防止分数发散。"},
				{Name: "bucket_q", Type: pluginapi.ConfigFieldTypeNumber, Description: "单个权重档位的相对宽度，例如 0.08 表示每档相差 8%。需按探测噪声标定：太窄会导致每轮都改权重、反复清空累加器。"},
				{Name: "hysteresis", Type: pluginapi.ConfigFieldTypeNumber, Description: "改变权重所需的超出量，以档位计，例如 0.35。数值越大越迟钝，写回越少。"},
				{Name: "warmup", Type: pluginapi.ConfigFieldTypeInteger, Description: "接管权重前需要连续成功探测的次数。未达标前插件不碰该凭据的 weight 字段。"},
				{Name: "freshness", Type: pluginapi.ConfigFieldTypeString, Description: "探测开始失败后，冻结的旧权重还能继续信任多久，例如 90m。超时后在状态页标记为 stale。"},
				{Name: "kickstart", Type: pluginapi.ConfigFieldTypeBoolean, Description: "周额度窗口从未启动过的账号，是否自动发一个最小推理请求把计时点着。这是本插件唯一会消耗额度的动作，默认关闭，dry_run 时不执行。"},
				{Name: "kickstart_short_window", Type: pluginapi.ConfigFieldTypeBoolean, Description: "五小时窗口从未启动过时，是否同样点火。独立开关，默认关闭。注意它不改变本插件算出的任何权重——短窗口只是可用性闸门，不参与打分；它换来的是该窗口提前起算、从而在后续爆发用量打满时更早重置。窗口每几小时自己过期，所以闲置账号会反复触发，按冷却计费。"},
				{Name: "kickstart_model", Type: pluginapi.ConfigFieldTypeString, Description: "点火请求使用的模型。Codex 会拒绝自家更小的模型，留空则用默认的 gpt-5.6-sol。"},
				{Name: "kickstart_cooldown", Type: pluginapi.ConfigFieldTypeString, Description: "同一账号两次点火的最短间隔，例如 1h，最小 1m。按尝试记账而非按成功——中途失败的请求可能已在上游计费。"},
				{Name: "error_penalty", Type: pluginapi.ConfigFieldTypeBoolean, Description: "账号自身原因的失败请求（401/403/5xx/网络错误等；不含 429、客户端断开和请求本身的错误）是否压低该账号权重。按 error_window 内的错误率等比打折，只在探测周期上生效，最低降到 1、绝不排除。默认关闭。"},
				{Name: "error_window", Type: pluginapi.ConfigFieldTypeString, Description: "错误率回看多久，例如 12h，默认 12h，不得短于 interval。窗口外的报错不再计入；越长惩罚保留越久，降权后回升途中的跨档写入也越分散。"},
				{Name: "reset_credits", Type: pluginapi.ConfigFieldTypeBoolean, Description: "是否把账号攒下的主动重置次数计入权重。每张未过期的重置券按「一整个周窗口、期限为其到期时间」叠加打分：券越多、越快到期，权重越高，好让该号先被用完再兑换。插件不会自动兑换。额度已耗尽的号有券也照样排除。开启后每轮每个号多一次带 token 的请求。默认关闭。"},
				{Name: "dry_run", Type: pluginapi.ConfigFieldTypeBoolean, Description: "空跑模式：照常探测并计算权重，但不写回任何凭据。首次部署建议开启。"},
			},
		},
		Capabilities: capabilities{ManagementAPI: true, UsagePlugin: true},
	}
}

// samePricing reports whether a committed bucket is still a valid coordinate.
// Only the inputs to Position and to the index-to-weight ladder qualify:
// Margin decides when a bucket moves, not where it sits, and clearing a bucket
// because hysteresis changed would bypass the very hysteresis being set.
// Cadence, dry-run, warmup, freshness and the endpoint never enter a score, and
// capacity scales the rate and the anchor by the same factor.
func samePricing(a, b pluginconf.Resolved) bool {
	x, y := a.Options.Policy, b.Options.Policy
	x.Quantizer.Margin, y.Quantizer.Margin = 0, 0
	return x.Params == y.Params && x.Quantizer == y.Quantizer
}

// reconfigure swaps in a new worker, carrying the engine's committed buckets
// across so a config reload does not re-price every credential and wipe the
// host's routing accumulator.
func reconfigure(request []byte) error {
	var req lifecycleRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	cfg, err := pluginconf.Decode(req.ConfigYAML)
	if err != nil {
		return err
	}

	mu.Lock()
	defer mu.Unlock()

	// The host reconfigures after every auth-file change, the plugin's own
	// write-back included. Rebuilding on an unchanged config would restart the
	// ticker, drop the burn baseline, and run an extra probe whose empty report
	// buries the one that recorded the write.
	if current != nil && current.cfg == cfg {
		return nil
	}

	previous := current
	if previous != nil {
		previous.halt()
	}
	next := &worker{
		cfg:  cfg,
		ctl:  controller.New(hostBridge{}, cfg.Options),
		stop: make(chan struct{}),
		done: make(chan struct{}),
		wake: make(chan struct{}, 1),
	}
	if previous != nil {
		// Both halves matter: Accounts keeps the committed buckets, Written
		// keeps what is already on disk. Dropping either makes the first cycle
		// after a reload rewrite every credential — and dropping Accounts also
		// drops the deadline that re-admits an excluded credential while its
		// probe is down, stranding it at the host's hard exclusion.
		engine := previous.ctl.Engine()
		accounts := engine.Accounts()
		if !samePricing(previous.cfg, cfg) {
			for id, account := range accounts {
				account.Bucket = balancer.Bucket{}
				accounts[id] = account
			}
		}
		next.ctl.Engine().Restore(accounts, engine.Written())
		// The cooldown is the only thing stopping a kickstart that never takes
		// from re-billing the account every cycle, and the host reconfigures
		// far more often than that cooldown is long.
		next.ctl.RestoreKickstarts(previous.ctl.Kickstarts())
		next.ctl.RestoreRechecks(previous.ctl.Rechecks())
		next.ctl.RestoreResetCredits(previous.ctl.ResetCredits())
	}
	current = next
	go next.loop()
	return nil
}

func stopRuntime() {
	mu.Lock()
	defer mu.Unlock()
	if current != nil {
		current.halt()
		current = nil
	}
}

func (w *worker) halt() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
}

func (w *worker) loop() {
	defer close(w.done)
	// halt() blocks on done, so a cycle started after the stop was signalled
	// holds the reconfigure or shutdown that signalled it — for as long as a
	// probe takes, and a probe has no overall deadline.
	select {
	case <-w.stop:
		return
	default:
	}
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	recovery := time.NewTimer(time.Hour)
	recovery.Stop()
	defer recovery.Stop()
	failed := w.cycle(false) != nil
	for {
		w.arm(recovery, failed)
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			failed = w.cycle(true) != nil
		case <-w.wake:
			failed = w.recheck() != nil
		case <-recovery.C:
			failed = w.recheck() != nil
		}
	}
}

// arm points the timer at the controller's next recheck.
func (w *worker) arm(timer *time.Timer, failed bool) {
	timer.Stop()
	now := time.Now().UTC()
	if at, ok := w.ctl.NextRecheck(now); ok {
		timer.Reset(recheckDelay(now, at, failed))
	}
}

// recheckDelay is how long to wait for a recheck due at at. One that failed
// learned nothing, and an overdue timer fired straight back at it spins on the
// host.
func recheckDelay(now, at time.Time, failed bool) time.Duration {
	delay := max(at.Sub(now), 0)
	if failed {
		delay = max(delay, controller.RecheckGap)
	}
	return delay
}

// reject notes a 429 and wakes the loop without waiting for it: the caller is
// the host's usage queue.
func (w *worker) reject(authIndex string) {
	if !w.ctl.NoteRejected(authIndex) {
		return
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *worker) recheck() error {
	if _, err := w.ctl.Recheck(time.Now().UTC()); err != nil {
		hostBridge{}.Log("warn", "recheck failed: "+err.Error())
		return err
	}
	return nil
}

// cycle runs one cycle; only a tick folds the request outcomes, so a refresh or
// a reload prices with the rate the last tick settled.
func (w *worker) cycle(tick bool) error {
	run := w.ctl.RunCycle
	if tick {
		run = w.ctl.Tick
	}
	report, err := run(time.Now().UTC())
	w.resultMu.Lock()
	w.lastRun, w.lastRep, w.lastErr = time.Now().UTC(), report, ""
	if err != nil {
		w.lastErr = err.Error()
	}
	w.resultMu.Unlock()
	if err != nil {
		hostBridge{}.Log("warn", "cycle failed: "+err.Error())
		return err
	}
	if report.Changed > 0 {
		hostBridge{}.Log("info", fmt.Sprintf("wrote %d of %d pending weight changes across %d credentials", report.Wrote, report.Changed, len(report.Decisions)))
	}
	return nil
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("decode management request: %w", err)
		}
	}

	// Held for the whole request. Releasing it here would let reconfigure or
	// shutdown swap the worker out from under a cycle that is already running,
	// leaving two engines writing weights against one host.
	mu.Lock()
	defer mu.Unlock()
	worker := current
	if worker == nil {
		return okEnvelope(jsonResponse(http.StatusServiceUnavailable, map[string]any{"error": "plugin is not configured"}))
	}

	switch {
	case strings.HasSuffix(req.Path, refreshPath) && req.Method == http.MethodPost:
		worker.cycle(false)
	case strings.HasSuffix(req.Path, statusPath):
	default:
		return okEnvelope(jsonResponse(http.StatusNotFound, map[string]any{"error": "unknown route"}))
	}
	return okEnvelope(jsonResponse(http.StatusOK, worker.status()))
}

// status reports state per credential. It deliberately carries no credential
// material and no file names — only the opaque auth index.
func (w *worker) status() map[string]any {
	w.resultMu.Lock()
	lastRun, lastErr, report := w.lastRun, w.lastErr, w.lastRep
	w.resultMu.Unlock()

	// The two budgets are listed apart because the switches are: an operator
	// running one of them needs to see which credentials it applies to.
	kicks := w.ctl.Kickstarts()
	weeklyDormant := make([]string, 0, len(kicks.Dormant))
	shortDormant := make([]string, 0, len(kicks.Dormant))
	for authIndex, dormancy := range kicks.Dormant {
		if dormancy.Weekly {
			weeklyDormant = append(weeklyDormant, authIndex)
		}
		if dormancy.Short {
			shortDormant = append(shortDormant, authIndex)
		}
	}
	slices.Sort(weeklyDormant)
	slices.Sort(shortDormant)

	accounts := w.ctl.Engine().Accounts()
	resets := w.ctl.ResetCredits()
	now := time.Now()
	rows := make([]map[string]any, 0, len(report.Decisions))
	for _, d := range report.Decisions {
		row := map[string]any{
			"auth_index": d.AuthID,
			"state":      stateName(d.State),
			"weight":     d.Weight,
			"degraded":   d.Degraded,
		}
		if d.Burn.Known {
			row["observed_burn_per_hour"] = d.Burn.Rate * 3600
		}
		if d.Error != "" {
			row["error"] = d.Error
		}
		if account, ok := accounts[d.AuthID]; ok {
			// A recheck moves an account between cycles, and this is the only
			// place that shows it before the next one.
			row["state"], row["weight"] = stateName(account.State), account.Weight
			row["stale"] = account.Stale
			if account.Bucket.Set {
				row["bucket"] = account.Bucket.Index
			}
			if w.cfg.Options.Policy.ErrorPenalty {
				row["error_rate"] = account.ErrorRate
			}
			if !account.RecoverAt.IsZero() {
				row["recover_at"] = account.RecoverAt.Format(time.RFC3339)
			}
		}
		if w.cfg.Options.ResetCredits {
			held := 0
			for _, expiry := range resets.Held[d.AuthID] {
				if expiry.After(now) {
					if held == 0 {
						row["reset_credit_next_expiry"] = expiry.Format(time.RFC3339)
					}
					held++
				}
			}
			row["reset_credits"] = held
			if err := resets.Errors[d.AuthID]; err != "" {
				row["reset_credits_error"] = err
			}
		}
		rows = append(rows, row)
	}

	out := map[string]any{
		"plugin":          pluginName,
		"version":         pluginVersion,
		"interval":        w.cfg.Interval.String(),
		"dry_run":         w.cfg.DryRun,
		"endpoint":        w.cfg.Options.Endpoint,
		"weights_pending": report.Changed,
		"weights_written": report.Wrote,
		"credentials":     rows,
		"capacity_scaled": w.cfg.Options.Capacity > 0,
		// Credentials that reported quota on a served request since the last
		// cycle and will therefore be priced without a probe. A hook that is
		// registered but never reached is otherwise indistinguishable from a
		// fleet that happens to be idle.
		"observations_pending": w.ctl.Pending(),
		// Credentials a 429 has queued for a probe off the timer.
		"rejections_pending": w.ctl.Rejections(),
		// Kickstart is the one thing here that spends quota, so its switches, its
		// effective model and what it has actually done are all reportable.
		"kickstart":              w.cfg.Options.Kickstart,
		"kickstart_short_window": w.cfg.Options.KickstartShortWindow,
		"kickstart_model":        w.cfg.Options.KickstartModel,
		"kickstarts_sent":        kicks.Sent,
		"error_penalty":          w.cfg.Options.Policy.ErrorPenalty,
		"error_window":           w.cfg.Options.Policy.ErrorWindow.String(),
		"reset_credits":          w.cfg.Options.ResetCredits,
	}
	if len(weeklyDormant) > 0 {
		out["dormant_credentials"] = weeklyDormant
	}
	if len(shortDormant) > 0 {
		out["short_dormant_credentials"] = shortDormant
	}
	if kicks.LastErr != "" {
		out["last_kickstart_error"] = kicks.LastErr
	}
	if !lastRun.IsZero() {
		out["last_run"] = lastRun.Format(time.RFC3339)
	}
	if last := w.ctl.LastRecheck(); !last.IsZero() {
		out["last_recheck"] = last.Format(time.RFC3339)
	}
	if lastErr != "" {
		out["last_error"] = lastErr
	}
	return out
}

func stateName(s balancer.State) string {
	switch s {
	case balancer.StateActive:
		return "active"
	case balancer.StateDraining:
		return "draining"
	case balancer.StateUnavailable:
		return "unavailable"
	default:
		return "pending"
	}
}

func jsonResponse(status int, payload any) managementResponse {
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		body = []byte(`{"error":"encode status"}`)
		status = http.StatusInternalServerError
	}
	return managementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}
