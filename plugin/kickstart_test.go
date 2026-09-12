package main

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/controller"
)

// host.http.do carries the request body as []byte, which JSON encodes as
// base64. A bridge that drops it turns every kickstart into a 400 that no
// controller test can see, because the controller's host is a fake.
func TestHostHTTPPayloadCarriesTheBody(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol"}`)
	raw, err := json.Marshal(httpDoPayload(controller.HTTPRequest{
		Method:  "POST",
		URL:     controller.KickstartEndpoint,
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    body,
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var sent struct {
		Method string `json:"method"`
		URL    string `json:"url"`
		Body   string `json:"body"`
	}
	if err = json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sent.Method != "POST" || sent.URL != controller.KickstartEndpoint {
		t.Fatalf("method %q url %q", sent.Method, sent.URL)
	}
	decoded, err := base64.StdEncoding.DecodeString(sent.Body)
	if err != nil {
		t.Fatalf("body is not base64: %v", err)
	}
	if string(decoded) != string(body) {
		t.Fatalf("body %q", decoded)
	}
}

// A GET carries no body, and an explicit empty one is not the same as an
// absent field to a host that validates its input.
func TestHostHTTPPayloadOmitsAnAbsentBody(t *testing.T) {
	raw, err := json.Marshal(httpDoPayload(controller.HTTPRequest{Method: "GET", URL: controller.UsageEndpoint}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "body") {
		t.Fatalf("probe payload carries a body: %s", raw)
	}
}

func TestStatusReportsKickstartActivity(t *testing.T) {
	configure(t, "interval: 15m\ndry_run: true")
	body := string(managementCall(t, "GET", pluginName+statusPath).Body)
	for _, want := range []string{`"kickstart"`, `"kickstarts_sent"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("status has no %s:\n%s", want, body)
		}
	}
	if !strings.Contains(body, `"kickstart": false`) {
		t.Fatalf("kickstart must report as off by default:\n%s", body)
	}
}

func TestStatusStillCarriesNoCredentialMaterialWithKickstartOn(t *testing.T) {
	configure(t, "interval: 15m\nkickstart: true\nkickstart_model: gpt-5.6-sol\ndry_run: true")
	body := string(managementCall(t, "GET", pluginName+statusPath).Body)
	for _, forbidden := range []string{"access_token", "Bearer", ".json", "account_id"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("status leaked %q:\n%s", forbidden, body)
		}
	}
	if !strings.Contains(body, `"kickstart_model": "gpt-5.6-sol"`) {
		t.Fatalf("status must surface the effective model:\n%s", body)
	}
}

// The host reconfigures on every auth-file change, its own write-back included.
// A rebuild that drops the cooldown would let a kickstart that never took be
// re-sent on the very next cycle.
func TestReconfigureCarriesKickstartStateAcross(t *testing.T) {
	configure(t, "interval: 15m\nkickstart: true\ndry_run: true")
	mu.Lock()
	ctl := current.ctl
	mu.Unlock()
	ctl.RestoreKickstarts(controller.KickstartState{
		Last:    map[string]time.Time{"abc": time.Now().UTC()},
		Dormant: map[string]bool{"abc": true},
		Sent:    3,
	})

	configure(t, "interval: 16m\nkickstart: true\ndry_run: true")
	body := string(managementCall(t, "GET", pluginName+statusPath).Body)
	if !strings.Contains(body, `"kickstarts_sent": 3`) {
		t.Fatalf("reload dropped the kickstart count:\n%s", body)
	}
	if !strings.Contains(body, `"abc"`) {
		t.Fatalf("reload dropped the dormant list:\n%s", body)
	}
}

func TestRegistrationDeclaresKickstartFields(t *testing.T) {
	var names []string
	for _, f := range pluginRegistration().Metadata.ConfigFields {
		names = append(names, f.Name)
	}
	for _, want := range []string{"kickstart", "kickstart_model", "kickstart_cooldown"} {
		if !slices.Contains(names, want) {
			t.Fatalf("registration has no %q field: %v", want, names)
		}
	}
}
