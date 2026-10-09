package codex

import (
	"encoding/json"
	"net/http"
	"strings"
)

// statusClientClosedRequest is what the host records for a request the client
// abandoned.
const statusClientClosedRequest = 499

// The host's own list of upstream complaints about the request rather than the
// credential (internal/clienterror, v7.3.2). The same request fails on any
// account, so holding it against the one that happened to carry it would steer
// traffic for nothing — and previous_response_not_found is what a client sees
// when a weight change evicts its sticky binding.
var (
	requestFaultCodes = map[string]bool{
		"cyber_policy": true, "context_length_exceeded": true, "message_too_big": true,
		"string_above_max_length": true, "invalid_prompt": true, "invalid_value": true,
		"unsupported_value": true, "invalid_request_error": true, "previous_response_not_found": true,
	}
	requestFaultTypes = map[string]bool{
		"invalid_request": true, "invalid_request_error": true, "bad_request_error": true, "invalid_prompt": true,
	}
)

// AccountFault reports whether a failed request counts against the credential
// that carried it. It mirrors the host's IsRequestFault, so the plugin blames an
// account for exactly what the host would rotate away from, minus two cases: a
// 429, which the recheck path answers with a probe, and a client that hung up.
func AccountFault(status int, body string) bool {
	switch status {
	case http.StatusTooManyRequests, statusClientClosedRequest:
		return false
	case http.StatusPaymentRequired:
		return true
	}
	if status == http.StatusUnauthorized && bodyMatches(body, typePaths, authenticationError) {
		return true
	}
	if bodyMatches(body, codePaths, modelNotFound) {
		return true
	}
	if bodyMatches(body, codePaths, requestFaultCodes) || bodyMatches(body, typePaths, requestFaultTypes) || itemNotPersisted(body) {
		return false
	}
	switch status {
	case http.StatusBadRequest, http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return false
	}
	return true
}

var (
	codePaths = [][]string{{"error", "code"}, {"code"}, {"response", "error", "code"}, {"body", "error", "code"}}
	typePaths = [][]string{{"error", "type"}, {"type"}, {"response", "error", "type"}, {"body", "error", "type"}}

	authenticationError = map[string]bool{"authentication_error": true}
	modelNotFound       = map[string]bool{"model_not_found": true, "model_not_found_error": true}
)

func bodyMatches(body string, paths [][]string, want map[string]bool) bool {
	for _, path := range paths {
		if want[bodyField(body, path)] {
			return true
		}
	}
	return false
}

// bodyField reads one string at path, or "" when the body is not JSON or the
// path does not lead to a string.
func bodyField(body string, path []string) string {
	raw := json.RawMessage(strings.TrimSpace(body))
	for _, key := range path {
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil {
			return ""
		}
		raw = object[key]
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(value))
}

// itemNotPersisted matches the plain-text 404 for a request that references an
// item the upstream never stored because store was false.
func itemNotPersisted(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "item with id") &&
		strings.Contains(lower, "not found") &&
		strings.Contains(lower, "items are not persisted when `store` is set to false")
}
