package protocol

import (
	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
)

// mutationMethods are the native methods that carry an idempotency_key. The read
// methods, initialize and service/shutdown have no mutation payload hash.
var mutationMethods = map[string]struct{}{
	"session/open":    {},
	"session/fork":    {},
	"turn/start":      {},
	"input/append":    {},
	"turn/interrupt":  {},
	"run/resume":      {},
	"context/compact": {},
}

// MutationPayloadHash is the identity of one native mutation:
//
//	D("rencrow-mutation/v1", principal, method, params without its top-level idempotency_key)
//
// principal is the authenticated connection profile's principal, never a value
// from the payload. params is the JSON text of the params object. Only the
// top-level idempotency_key is removed; a nested field of the same name, the
// OriginProof, limits, expected revisions and every null stay part of the hash.
// RPC id, time and the current policy snapshot are not part of it.
func MutationPayloadHash(principal, method string, params []byte) (string, error) {
	if err := ValidatePrincipal(principal); err != nil {
		return "", err
	}
	if _, ok := mutationMethods[method]; !ok {
		return "", ErrNotMutationMethod
	}
	v, err := strictjson.Decode(params)
	if err != nil {
		return "", invalidWrap(err, "mutation params")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return "", invalid("mutation params must be a JSON object")
	}
	if key, ok := obj["idempotency_key"].(string); !ok || key == "" {
		return "", invalid("mutation params need a non-empty string idempotency_key")
	}
	delete(obj, "idempotency_key")
	digest, err := canon.D("rencrow-mutation/v1", principal, method, obj)
	if err != nil {
		return "", invalidWrap(err, "mutation params")
	}
	return digest, nil
}
