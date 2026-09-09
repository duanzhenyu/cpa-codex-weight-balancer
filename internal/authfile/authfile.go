// Package authfile edits a credential JSON document in place.
//
// host.auth.save takes the whole document and offers no compare-and-swap, so a
// write races with anything else touching the file — a token refresh, another
// plugin, the management UI. The only defence is to re-read immediately before
// writing and to carry every key we did not mean to change through untouched.
package authfile

import (
	"encoding/json"
	"fmt"
)

const weightKey = "weight"

// Weight reads the credential's current weight.
func Weight(raw []byte) (int64, bool) {
	fields, err := decodeObject(raw)
	if err != nil {
		return 0, false
	}
	value, ok := fields[weightKey]
	if !ok {
		return 0, false
	}
	var weight int64
	if json.Unmarshal(value, &weight) != nil {
		return 0, false
	}
	return weight, true
}

// SetWeight returns the document with weight set, preserving every other key.
// Values we do not touch are carried across as raw JSON, so a nested token
// object survives byte-for-byte.
func SetWeight(raw []byte, weight int64) ([]byte, error) {
	fields, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(weight)
	if err != nil {
		return nil, fmt.Errorf("encode weight: %w", err)
	}
	fields[weightKey] = encoded
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode credential: %w", err)
	}
	return out, nil
}

func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("decode credential: %w", err)
	}
	if fields == nil {
		return nil, fmt.Errorf("credential is not a JSON object")
	}
	return fields, nil
}
