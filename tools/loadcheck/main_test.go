package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const goodRegistration = `{"ok":true,"result":{"schema_version":5,"metadata":{"Name":"cpa-codex-weight-balancer","Version":"0.7.1","Author":"duanzhenyu","GitHubRepository":"https://github.com/duanzhenyu/cpa-codex-weight-balancer"},"capabilities":{"management_api":true,"usage_plugin":true}}}`

func TestValidateAcceptsWhatTheHostAccepts(t *testing.T) {
	if err := validate([]byte(goodRegistration), "0.7.1"); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
}

func TestCheckTable(t *testing.T) {
	if err := checkTable(pluginabi.ABIVersion, true, true, true); err != nil {
		t.Fatalf("checkTable(complete) = %v, want nil", err)
	}
	for name, err := range map[string]error{
		"other abi":   checkTable(pluginabi.ABIVersion+1, true, true, true),
		"no call":     checkTable(pluginabi.ABIVersion, false, true, true),
		"no free":     checkTable(pluginabi.ABIVersion, true, false, true),
		"no shutdown": checkTable(pluginabi.ABIVersion, true, true, false),
	} {
		if err == nil {
			t.Errorf("checkTable(%s) = nil, want an error", name)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]struct {
		raw, version, want string
	}{
		"other version":       {goodRegistration, "0.7.0", "version"},
		"no author":           {strings.Replace(goodRegistration, `"duanzhenyu"`, `" "`, 1), "0.7.1", "Author"},
		"no repository":       {strings.Replace(goodRegistration, `"https://github.com/duanzhenyu/cpa-codex-weight-balancer"`, `""`, 1), "0.7.1", "GitHubRepository"},
		"no name":             {strings.Replace(goodRegistration, `"Name":"cpa-codex-weight-balancer"`, `"Name":""`, 1), "0.7.1", "Name"},
		"no management route": {strings.Replace(goodRegistration, `"management_api":true`, `"management_api":false`, 1), "0.7.1", "management_api"},
		"no usage hook":       {strings.Replace(goodRegistration, `"usage_plugin":true`, `"usage_plugin":false`, 1), "0.7.1", "usage_plugin"},
		"schema too new":      {strings.Replace(goodRegistration, `"schema_version":5`, `"schema_version":99`, 1), "0.7.1", "schema"},
		"error envelope":      {`{"ok":false,"error":{"code":"plugin_error","message":"bad config"}}`, "0.7.1", "bad config"},
		"not json":            {`not json`, "0.7.1", "decode"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := validate([]byte(tc.raw), tc.version)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate() = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// fakeRefresh answers the refresh route, calling back into the host first when
// callback is set, as the real plugin's cycle does.
type fakeRefresh struct {
	raw      string
	callback bool
}

func (f fakeRefresh) call(method string, request []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	if method != pluginabi.MethodManagementHandle || req.Method != "POST" || !strings.HasSuffix(req.Path, "/refresh") {
		return nil, fmt.Errorf("unexpected call %s %+v", method, req)
	}
	if f.callback {
		onHostCall()
	}
	return []byte(f.raw), nil
}

func (fakeRefresh) shutdown() error { return nil }

// fakePlugin registers, refreshes and reports err from its shutdown.
type fakePlugin struct {
	shutdowns *int
	err       error
}

func (f fakePlugin) call(method string, request []byte) ([]byte, error) {
	if method == pluginabi.MethodPluginRegister {
		return []byte(goodRegistration), nil
	}
	return fakeRefresh{`{"ok":true,"result":{"StatusCode":200}}`, true}.call(method, request)
}

func (f fakePlugin) shutdown() error {
	*f.shutdowns++
	return f.err
}

func TestCheckReloadsAndFailsOnShutdown(t *testing.T) {
	cases := map[string]struct {
		err                      error
		wantLoads, wantShutdowns int
	}{
		"clean":    {nil, 2, 2},
		"unmapped": {errors.New("unmapped"), 1, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var loads, shutdowns int
			err := check(func() (library, error) {
				loads++
				return fakePlugin{&shutdowns, tc.err}, nil
			}, "0.7.1")
			if err != tc.err || loads != tc.wantLoads || shutdowns != tc.wantShutdowns {
				t.Fatalf("check() = %v after %d loads and %d shutdowns, want %v after %d and %d",
					err, loads, shutdowns, tc.err, tc.wantLoads, tc.wantShutdowns)
			}
		})
	}
}

func TestRefreshRequiresAHostCallback(t *testing.T) {
	// A callback before the probe must not count for it.
	onHostCall()
	ok := `{"ok":true,"result":{"StatusCode":200}}`
	cases := map[string]struct {
		lib  fakeRefresh
		want string
	}{
		"callback":     {fakeRefresh{ok, true}, ""},
		"no callback":  {fakeRefresh{ok, false}, "host callback"},
		"not json":     {fakeRefresh{`not json`, true}, "decode"},
		"failed":       {fakeRefresh{`{"ok":false,"error":{"code":"x","message":"boom"}}`, true}, "boom"},
		"other status": {fakeRefresh{`{"ok":true,"result":{"StatusCode":404}}`, true}, "404"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := refresh(tc.lib)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refresh() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refresh() = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}
