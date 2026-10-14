// Command loadcheck loads a built plugin library the way the CPA host does,
// registers it, runs one cycle that calls back into the host, and shuts it
// down. A library that compiles for a platform says nothing about whether the
// host there can load it.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// library is a plugin opened and initialised against a host whose every
// callback goes through onHostCall and fails.
type library interface {
	call(method string, request []byte) ([]byte, error)
	shutdown() error
}

func main() {
	version := flag.String("version", "", "plugin version the library must report")
	flag.Parse()
	if flag.NArg() != 1 || *version == "" {
		fmt.Fprintln(os.Stderr, "usage: loadcheck -version <x.y.z> <library>")
		os.Exit(2)
	}
	if err := check(func() (library, error) { return open(flag.Arg(0)) }, *version); err != nil {
		fmt.Fprintln(os.Stderr, "loadcheck:", err)
		os.Exit(1)
	}
	fmt.Println("loadcheck: ok", flag.Arg(0))
}

// check goes through the plugin's life twice, as the host does when a plugin
// is disabled and enabled again: the second load initialises the image the
// first one left behind.
func check(load func() (library, error), version string) error {
	for range 2 {
		if err := cycle(load, version); err != nil {
			return err
		}
	}
	return nil
}

func cycle(load func() (library, error), version string) (err error) {
	lib, err := load()
	if err != nil {
		return err
	}
	defer func() {
		if serr := lib.shutdown(); err == nil {
			err = serr
		}
	}()
	request, err := json.Marshal(map[string]any{
		"config_yaml":    []byte("enabled: true\npriority: 0\ndry_run: true\n"),
		"schema_version": pluginabi.SchemaVersion,
	})
	if err != nil {
		return err
	}
	raw, err := lib.call(pluginabi.MethodPluginRegister, request)
	if err != nil {
		return err
	}
	if err := validate(raw, version); err != nil {
		return err
	}
	return refresh(lib)
}

var hostCalls atomic.Uint64

// onHostCall is where every host callback lands. The host answers callbacks in
// Go, so each one re-enters this runtime from inside a plugin call; growing the
// stack there is the transition that broke the host's Windows loader once.
func onHostCall() {
	_ = growStack(64)
	hostCalls.Add(1)
}

//go:noinline
func growStack(depth int) int {
	var pad [1024]byte
	for i := range pad {
		pad[i] = byte(i + depth)
	}
	if depth == 0 {
		return int(pad[0])
	}
	return growStack(depth-1) + int(pad[depth%len(pad)])
}

// refresh runs one cycle synchronously, which lists credentials through the
// host. The worker that registration starts may be stopped before it calls
// back at all, so it proves nothing about callbacks.
func refresh(lib library) error {
	request, err := json.Marshal(pluginapi.ManagementRequest{Method: "POST", Path: "/cpa-codex-weight-balancer/refresh"})
	if err != nil {
		return err
	}
	before := hostCalls.Load()
	raw, err := lib.call(pluginabi.MethodManagementHandle, request)
	if err != nil {
		return err
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode refresh envelope: %w", err)
	}
	if !env.OK {
		if env.Error != nil {
			return fmt.Errorf("refresh failed: %s: %s", env.Error.Code, env.Error.Message)
		}
		return errors.New("refresh failed")
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return fmt.Errorf("decode refresh response: %w", err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("refresh answered HTTP %d", resp.StatusCode)
	}
	if hostCalls.Load() == before {
		return errors.New("refresh made no host callback")
	}
	return nil
}

// checkTable applies the host's checks on the table cliproxy_plugin_init fills,
// plus shutdown, without which the plugin's worker outlives an unload.
func checkTable(abi uint32, call, free, shutdown bool) error {
	if abi != pluginabi.ABIVersion {
		return fmt.Errorf("plugin ABI version %d, host speaks %d", abi, pluginabi.ABIVersion)
	}
	if !call || !free || !shutdown {
		return errors.New("plugin function table is incomplete")
	}
	return nil
}

func validate(raw []byte, version string) error {
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode register envelope: %w", err)
	}
	if !env.OK {
		if env.Error != nil {
			return fmt.Errorf("register failed: %s: %s", env.Error.Code, env.Error.Message)
		}
		return errors.New("register failed")
	}
	var reg struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
		Capabilities  struct {
			ManagementAPI bool `json:"management_api"`
			UsagePlugin   bool `json:"usage_plugin"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		return fmt.Errorf("decode registration: %w", err)
	}
	if reg.SchemaVersion > pluginabi.SchemaVersion {
		return fmt.Errorf("schema version %d is newer than the host's %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	// The host drops a registration missing any of these and logs only
	// "invalid metadata or no capabilities", without naming the field.
	m := reg.Metadata
	for _, f := range []struct{ name, value string }{
		{"Name", m.Name}, {"Version", m.Version}, {"Author", m.Author}, {"GitHubRepository", m.GitHubRepository},
	} {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("metadata %s is empty", f.name)
		}
	}
	if m.Version != version {
		return fmt.Errorf("version %q, want %q", m.Version, version)
	}
	if !reg.Capabilities.ManagementAPI {
		return errors.New("management_api capability is not declared")
	}
	if !reg.Capabilities.UsagePlugin {
		return errors.New("usage_plugin capability is not declared")
	}
	return nil
}
