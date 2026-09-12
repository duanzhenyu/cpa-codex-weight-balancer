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
	pluginVersion = "0.3.0"
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
				{Name: "interval", Type: pluginapi.ConfigFieldTypeString, Description: "探测间隔，例如 15m。每次权重变动都会清空宿主全队列的路由累加器，务必保持在几十分钟量级。最小 1m。"},
				{Name: "endpoint", Type: pluginapi.ConfigFieldTypeString, Description: "额度探测地址。必须是 https —— 每次探测都带着凭据的 access_token。留空则用默认的 Codex usage 端点。"},
				{Name: "quota_capacity", Type: pluginapi.ConfigFieldTypeNumber, Description: "套餐总量，把 used_percent 与观测消耗换算成绝对单位。它是全队伍共用的一个值，不能让不同套餐变得可比，也不改变路由比例。"},
				{Name: "alpha", Type: pluginapi.ConfigFieldTypeNumber, Description: "紧迫度指数。1 表示按周窗口的 deadline 匀速配速；0 表示只看剩余额度、不管刷新时间；大于 1 则更偏向快要刷新的账号。"},
				{Name: "t_floor", Type: pluginapi.ConfigFieldTypeString, Description: "距刷新时间的下限，例如 15m。窗口即将关闭时防止分数发散。"},
				{Name: "bucket_q", Type: pluginapi.ConfigFieldTypeNumber, Description: "单个权重档位的相对宽度，例如 0.08 表示每档相差 8%。需按探测噪声标定：太窄会导致每轮都改权重、反复清空累加器。"},
				{Name: "hysteresis", Type: pluginapi.ConfigFieldTypeNumber, Description: "改变权重所需的超出量，以档位计，例如 0.35。数值越大越迟钝，写回越少。"},
				{Name: "warmup", Type: pluginapi.ConfigFieldTypeInteger, Description: "接管权重前需要连续成功探测的次数。未达标前插件不碰该凭据的 weight 字段。"},
				{Name: "freshness", Type: pluginapi.ConfigFieldTypeString, Description: "探测开始失败后，冻结的旧权重还能继续信任多久，例如 90m。超时后在状态页标记为 stale。"},
				{Name: "kickstart", Type: pluginapi.ConfigFieldTypeBoolean, Description: "周额度窗口从未启动过的账号，是否自动发一个最小推理请求把计时点着。这是本插件唯一会消耗额度的动作，默认关闭，dry_run 时不执行。"},
				{Name: "kickstart_model", Type: pluginapi.ConfigFieldTypeString, Description: "点火请求使用的模型。Codex 会拒绝自家更小的模型，留空则用默认的 gpt-5.6-sol。"},
				{Name: "kickstart_cooldown", Type: pluginapi.ConfigFieldTypeString, Description: "同一账号两次点火的最短间隔，例如 1h，最小 1m。按尝试记账而非按成功——中途失败的请求可能已在上游计费。"},
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
	w.cycle()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			w.cycle()
		}
	}
}

func (w *worker) cycle() {
	report, err := w.ctl.RunCycle(time.Now().UTC())
	w.resultMu.Lock()
	w.lastRun, w.lastRep, w.lastErr = time.Now().UTC(), report, ""
	if err != nil {
		w.lastErr = err.Error()
	}
	w.resultMu.Unlock()
	if err != nil {
		hostBridge{}.Log("warn", "cycle failed: "+err.Error())
		return
	}
	if report.Changed > 0 {
		hostBridge{}.Log("info", fmt.Sprintf("wrote %d of %d pending weight changes across %d credentials", report.Wrote, report.Changed, len(report.Decisions)))
	}
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
		worker.cycle()
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

	kicks := w.ctl.Kickstarts()
	dormant := make([]string, 0, len(kicks.Dormant))
	for authIndex, isDormant := range kicks.Dormant {
		if isDormant {
			dormant = append(dormant, authIndex)
		}
	}
	slices.Sort(dormant)

	accounts := w.ctl.Engine().Accounts()
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
			row["stale"] = account.Stale
			if account.Bucket.Set {
				row["bucket"] = account.Bucket.Index
			}
			if !account.RecoverAt.IsZero() {
				row["recover_at"] = account.RecoverAt.Format(time.RFC3339)
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
		// Kickstart is the one thing here that spends quota, so its switch, its
		// effective model and what it has actually done are all reportable.
		"kickstart":       w.cfg.Options.Kickstart,
		"kickstart_model": w.cfg.Options.KickstartModel,
		"kickstarts_sent": kicks.Sent,
	}
	if len(dormant) > 0 {
		out["dormant_credentials"] = dormant
	}
	if kicks.LastErr != "" {
		out["last_kickstart_error"] = kicks.LastErr
	}
	if !lastRun.IsZero() {
		out["last_run"] = lastRun.Format(time.RFC3339)
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
