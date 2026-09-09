package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"gitlab.duanzhenyu.top/dzy/cpa-codex-weight-balancer/internal/balancer"
	"gitlab.duanzhenyu.top/dzy/cpa-codex-weight-balancer/internal/controller"
	"gitlab.duanzhenyu.top/dzy/cpa-codex-weight-balancer/internal/pluginconf"
)

const (
	pluginName    = "cpa-codex-weight-balancer"
	pluginVersion = "0.1.0"
	statusPath    = "/status"
	refreshPath   = "/refresh"
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
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:    pluginName,
			Version: pluginVersion,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "interval", Type: pluginapi.ConfigFieldTypeString, Description: "Probe cadence, e.g. 15m. Each weight change wipes the host routing accumulator, so keep this in the tens of minutes."},
				{Name: "endpoint", Type: pluginapi.ConfigFieldTypeString, Description: "Codex usage probe URL."},
				{Name: "quota_capacity", Type: pluginapi.ConfigFieldTypeNumber, Description: "Plan capacity used to turn used_percent into a tier-comparable unit. Leave unset when every account is on the same plan."},
				{Name: "alpha", Type: pluginapi.ConfigFieldTypeNumber, Description: "Urgency exponent. 1 paces to the weekly deadline; 0 weights by remaining quota alone; above 1 favours accounts closest to their reset."},
				{Name: "t_floor", Type: pluginapi.ConfigFieldTypeString, Description: "Lower clamp on time-to-reset, e.g. 15m. Stops the score diverging as a window closes."},
				{Name: "bucket_q", Type: pluginapi.ConfigFieldTypeNumber, Description: "Relative width of one weight bucket, e.g. 0.08. Calibrate against probe noise."},
				{Name: "hysteresis", Type: pluginapi.ConfigFieldTypeNumber, Description: "Overshoot in buckets required before a weight moves, e.g. 0.35."},
				{Name: "warmup", Type: pluginapi.ConfigFieldTypeInteger, Description: "Consecutive good probes required before the plugin takes over a credential's weight."},
				{Name: "freshness", Type: pluginapi.ConfigFieldTypeString, Description: "How long a frozen weight is trusted after probes start failing, e.g. 90m."},
				{Name: "dry_run", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Compute and report weights without writing them back."},
			},
		},
		Capabilities: capabilities{ManagementAPI: true},
	}
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
		// after a reload rewrite every credential.
		engine := previous.ctl.Engine()
		next.ctl.Engine().Restore(engine.Accounts(), engine.Written())
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
		"plugin":            pluginName,
		"version":           pluginVersion,
		"interval":          w.cfg.Interval.String(),
		"dry_run":           w.cfg.DryRun,
		"endpoint":          w.cfg.Options.Endpoint,
		"weights_pending":   report.Changed,
		"weights_written":   report.Wrote,
		"credentials":       rows,
		"accumulator_wiped": report.Wrote > 0,
		"tier_comparable":   w.cfg.Options.Capacity > 0,
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
