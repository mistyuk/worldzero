package action

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"

	"github.com/mistyuk/worldzero/internal/kernel/werr"
)

// strictUnmarshal decodes params, rejecting unknown fields.
//
// Strictness is a feature for agents specifically. An LLM-driven runner that
// misspells a parameter, or invents one it expects to exist, gets told
// immediately rather than watching the action succeed while quietly ignoring
// what it asked for. Silence there is the worst outcome: the agent updates its
// model of the world with a belief that is wrong.
//
// It is also a security property. A body that can carry unknown fields is a body
// that can carry `owner_user_id` or `agent_id` in the hope something reads it.
func strictUnmarshal(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return werr.New(werr.InvalidParams, "params are not valid for this action")
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return werr.New(werr.InvalidParams, "params must be exactly one JSON object")
	}
	return nil
}

// fingerprint computes the canonical request hash without executing anything.
//
// Used by the replay path to detect "same key, different body". It re-marshals
// the DECODED params rather than hashing raw bytes, so that a retry differing
// only in key order or whitespace is recognised as the same request — hashing
// the wire bytes would reject an honest retry as a conflict.
func (h handler[P]) fingerprint(raw json.RawMessage) ([]byte, error) {
	var p P
	if len(raw) > 0 {
		if err := strictUnmarshal(raw, &p); err != nil {
			return nil, err
		}
	}
	canonical, err := json.Marshal(p)
	if err != nil {
		return nil, werr.Wrap(werr.Internal, "could not process parameters", err)
	}
	sum := sha256.Sum256(append(append([]byte(h.v.Type), 0), canonical...))
	return sum[:], nil
}
