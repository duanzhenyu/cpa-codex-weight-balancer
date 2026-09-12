package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/duanzhenyu/cpa-codex-weight-balancer/internal/controller"
)

// hostBridge implements controller.Host on top of the host callbacks.
type hostBridge struct{}

func (h hostBridge) AuthList() ([]controller.AuthEntry, error) {
	result, err := callHost(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err = json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("decode host.auth.list: %w", err)
	}
	entries := make([]controller.AuthEntry, 0, len(resp.Files))
	for _, file := range resp.Files {
		kind := strings.TrimSpace(file.Type)
		if kind == "" {
			kind = strings.TrimSpace(file.Provider)
		}
		entries = append(entries, controller.AuthEntry{
			AuthIndex:   file.AuthIndex,
			Name:        file.Name,
			Type:        kind,
			Disabled:    file.Disabled,
			Unavailable: file.Unavailable,
		})
	}
	return entries, nil
}

func (h hostBridge) AuthGet(authIndex string) (string, []byte, error) {
	result, err := callHost(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return "", nil, err
	}
	var resp pluginapi.HostAuthGetResponse
	if err = json.Unmarshal(result, &resp); err != nil {
		return "", nil, fmt.Errorf("decode host.auth.get: %w", err)
	}
	return resp.Name, resp.JSON, nil
}

func (h hostBridge) AuthSave(name string, raw []byte) error {
	_, err := callHost(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
		Name: name,
		JSON: json.RawMessage(raw),
	})
	return err
}

func (h hostBridge) HTTPDo(req controller.HTTPRequest) (controller.HTTPResponse, error) {
	result, err := callHost(pluginabi.MethodHostHTTPDo, httpDoPayload(req))
	if err != nil {
		return controller.HTTPResponse{}, err
	}
	var resp struct {
		StatusCode int
		Body       []byte
	}
	if err = json.Unmarshal(result, &resp); err != nil {
		return controller.HTTPResponse{}, fmt.Errorf("decode host.http.do: %w", err)
	}
	return controller.HTTPResponse{StatusCode: resp.StatusCode, Body: resp.Body}, nil
}

// httpDoPayload shapes one outbound call for the host. The body is omitted
// rather than sent empty: the probe has none, and the host decodes this field
// into a []byte it hands straight to its HTTP client.
func httpDoPayload(req controller.HTTPRequest) map[string]any {
	payload := map[string]any{
		"method":  req.Method,
		"url":     req.URL,
		"headers": req.Headers,
	}
	if len(req.Body) > 0 {
		payload["body"] = req.Body
	}
	return payload
}

// Log never carries credential material: the message is composed from auth
// indices and weights only.
func (h hostBridge) Log(level, message string) {
	_, _ = callHost(pluginabi.MethodHostLog, map[string]any{
		"level":   level,
		"message": pluginName + ": " + message,
	})
}
