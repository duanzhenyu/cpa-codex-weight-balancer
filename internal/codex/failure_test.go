package codex

import (
	"net/http"
	"testing"
)

func TestAccountFaultFollowsTheHostRequestFaultRule(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   bool
	}{
		"server error":           {http.StatusInternalServerError, "upstream exploded", true},
		"bad gateway":            {http.StatusBadGateway, "", true},
		"unavailable":            {http.StatusServiceUnavailable, "", true},
		"gateway timeout":        {http.StatusGatewayTimeout, "context deadline exceeded", true},
		"transport, no status":   {0, "connection reset by peer", true},
		"unauthorized":           {http.StatusUnauthorized, `{"error":{"message":"token expired"}}`, true},
		"forbidden":              {http.StatusForbidden, "", true},
		"payment required":       {http.StatusPaymentRequired, `{"error":{"type":"invalid_request_error"}}`, true},
		"model not found on 400": {http.StatusBadRequest, `{"error":{"code":"model_not_found","type":"invalid_request_error"}}`, true},
		"auth error with generic code": {http.StatusUnauthorized,
			`{"error":{"code":"invalid_request_error","type":"authentication_error"}}`, true},
		"non-object error field": {http.StatusInternalServerError, `{"error":"boom"}`, true},

		// 429 is the recheck path's to act on, not a reason to reprice.
		"rate limited":            {http.StatusTooManyRequests, "", false},
		"client closed":           {499, "context canceled", false},
		"bad request":             {http.StatusBadRequest, "", false},
		"conflict":                {http.StatusConflict, "", false},
		"too large":               {http.StatusRequestEntityTooLarge, "", false},
		"unprocessable":           {http.StatusUnprocessableEntity, "", false},
		"context length":          {http.StatusInternalServerError, `{"error":{"code":"context_length_exceeded"}}`, false},
		"lost previous response":  {http.StatusNotFound, `{"error":{"code":"previous_response_not_found"}}`, false},
		"top-level code":          {http.StatusNotFound, `{"code":"Invalid_Prompt"}`, false},
		"wrapped response error":  {http.StatusBadGateway, `{"response":{"error":{"type":"invalid_request_error"}}}`, false},
		"wrapped body error":      {http.StatusBadGateway, `{"body":{"error":{"code":"cyber_policy"}}}`, false},
		"top-level type":          {http.StatusServiceUnavailable, `{"type":"bad_request_error"}`, false},
		"item not persisted":      {http.StatusNotFound, "Item with id 'rs_1' not found. Items are not persisted when `store` is set to false.", false},
		"unauthorized, req fault": {http.StatusUnauthorized, `{"error":{"code":"invalid_request_error"}}`, false},
	} {
		if got := AccountFault(tc.status, tc.body); got != tc.want {
			t.Errorf("%s: AccountFault(%d, %q) = %v, want %v", name, tc.status, tc.body, got, tc.want)
		}
	}
}
