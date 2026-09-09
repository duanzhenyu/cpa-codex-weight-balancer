package authfile

import (
	"encoding/json"
	"strings"
	"testing"
)

const credential = `{
  "type": "codex",
  "email": "user@example.com",
  "token": {"access_token": "secret", "refresh_token": "also-secret", "expire": "2026-09-09T00:00:00Z"},
  "last_refresh": "2026-09-08T00:00:00Z",
  "weighted_average": 3
}`

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	return out
}

func TestSetWeightAddsField(t *testing.T) {
	out, err := SetWeight([]byte(credential), 4661)
	if err != nil {
		t.Fatal(err)
	}
	got := decode(t, out)
	if got["weight"] != float64(4661) {
		t.Fatalf("weight = %v", got["weight"])
	}
}

func TestSetWeightPreservesEveryOtherKey(t *testing.T) {
	out, err := SetWeight([]byte(credential), 7)
	if err != nil {
		t.Fatal(err)
	}
	got := decode(t, out)
	if got["type"] != "codex" || got["email"] != "user@example.com" {
		t.Fatalf("scalar keys lost: %v", got)
	}
	if got["last_refresh"] != "2026-09-08T00:00:00Z" {
		t.Fatalf("last_refresh lost: %v", got["last_refresh"])
	}
	// A nested token object must survive intact: clobbering it would log the
	// account out, and it is exactly what a concurrent refresh rewrites.
	token, ok := got["token"].(map[string]any)
	if !ok || token["access_token"] != "secret" || token["refresh_token"] != "also-secret" {
		t.Fatalf("token object mangled: %v", got["token"])
	}
	// A key that merely starts with "weight" is not the weight.
	if got["weighted_average"] != float64(3) {
		t.Fatalf("weighted_average = %v", got["weighted_average"])
	}
}

func TestSetWeightReplacesExisting(t *testing.T) {
	out, err := SetWeight([]byte(`{"type":"codex","weight":12}`), 99)
	if err != nil {
		t.Fatal(err)
	}
	if got := decode(t, out); got["weight"] != float64(99) {
		t.Fatalf("weight = %v", got["weight"])
	}
	if strings.Count(string(out), `"weight"`) != 1 {
		t.Fatalf("duplicate weight key: %s", out)
	}
}

func TestSetWeightZeroIsWritten(t *testing.T) {
	// 0 is a real decision — hard exclusion — not an absent value.
	out, err := SetWeight([]byte(`{"type":"codex","weight":50}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := decode(t, out)["weight"]
	if !ok || raw != float64(0) {
		t.Fatalf("weight = %v present=%v", raw, ok)
	}
}

func TestSetWeightRejectsNonObject(t *testing.T) {
	for _, body := range []string{`[1,2]`, `"text"`, `12`, `null`, `not json`, ``} {
		if _, err := SetWeight([]byte(body), 1); err == nil {
			t.Fatalf("%q must be rejected", body)
		}
	}
}

func TestWeightReads(t *testing.T) {
	if w, ok := Weight([]byte(`{"weight":123}`)); !ok || w != 123 {
		t.Fatalf("got %d ok=%v", w, ok)
	}
	if _, ok := Weight([]byte(credential)); ok {
		t.Fatal("no weight key means no weight")
	}
	if _, ok := Weight([]byte(`{"weight":"12"}`)); ok {
		t.Fatal("a string weight is not an integer weight")
	}
	if _, ok := Weight([]byte(`nope`)); ok {
		t.Fatal("invalid JSON has no weight")
	}
}

func TestSetWeightRoundTrips(t *testing.T) {
	out, err := SetWeight([]byte(credential), 42)
	if err != nil {
		t.Fatal(err)
	}
	again, err := SetWeight(out, 42)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(out) {
		t.Fatalf("not idempotent:\n%s\n%s", out, again)
	}
	if w, ok := Weight(again); !ok || w != 42 {
		t.Fatalf("got %d ok=%v", w, ok)
	}
}
