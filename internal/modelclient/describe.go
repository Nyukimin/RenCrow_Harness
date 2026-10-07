package modelclient

import (
	"context"
	"net/http"

	"github.com/Nyukimin/RenCrow_Harness/internal/modelport"
	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// unverifiedFingerprint is the binding_fingerprint the Gateway states for a binding
// whose identity it cannot verify. It is not a fingerprint: no descriptor may carry
// it, and it equals no expected value.
const unverifiedFingerprint = "unverified"

// Describe asks GET /status for the descriptor of the binding. The Gateway lists in
// contracts.harness-v1.bindings only the bindings whose identity, count and stage
// options it has verified, so a binding that is not listed is not one the Harness
// may use: it is UNSUPPORTED_CONTRACT, never an assumed default.
//
// Only the capability advertisement and the one descriptor that names the binding
// are read; another binding's entry is not this call's concern. The descriptor is
// read strictly: every field of the contract, none extra.
//
// An answer that is not HTTP 200, or a Gateway that cannot be reached, is not a
// strict refusal and comes back as an error that is not a *modelport.StrictError.
func (c *Client) Describe(ctx context.Context, b protocol.Binding) (modelport.BindingDescriptor, error) {
	ex, err := c.roundTrip(ctx, "describe", http.MethodGet, "/status", nil, "application/json")
	if err != nil {
		return modelport.BindingDescriptor{}, err
	}
	defer ex.resp.Body.Close()
	if ex.resp.StatusCode != http.StatusOK {
		return modelport.BindingDescriptor{}, errf("describe: the Gateway status answered HTTP %d", ex.resp.StatusCode)
	}
	body, over, err := readBody(ex.resp.Body, maxStatusBytes)
	if err != nil {
		return modelport.BindingDescriptor{}, &TransportError{Op: "describe", Connected: true, Err: cause(err)}
	}
	if over {
		return modelport.BindingDescriptor{}, contractFailed(nil, "the Gateway status is over the size limit")
	}
	v, err := strictjson.Decode(body)
	if err != nil {
		return modelport.BindingDescriptor{}, contractFailed(nil, "the Gateway status is not strict JSON")
	}
	root, ok := v.(map[string]any)
	if !ok {
		return modelport.BindingDescriptor{}, contractFailed(nil, "the Gateway status is not an object")
	}
	contracts, present := root["contracts"]
	if !present {
		return modelport.BindingDescriptor{}, unsupported("the Gateway does not advertise any contract")
	}
	all, ok := contracts.(map[string]any)
	if !ok {
		return modelport.BindingDescriptor{}, contractFailed(nil, "the Gateway contracts are not an object")
	}
	capability, present := all[modelport.ContractVersion]
	if !present {
		return modelport.BindingDescriptor{}, unsupported("the Gateway does not advertise " + modelport.ContractVersion)
	}
	return pickDescriptor(capability, b)
}

func unsupported(message string) *modelport.StrictError {
	return &modelport.StrictError{Code: modelport.CodeUnsupportedContract, Message: message}
}

// pickDescriptor checks the harness-v1 advertisement and returns the descriptor of
// the binding. The advertisement is held to what this client needs (strict-v1
// normalization, measure, receipts, explicit recovery, no generation retry); a
// member it does not know is not a fault of the advertisement.
func pickDescriptor(capability any, b protocol.Binding) (modelport.BindingDescriptor, error) {
	adv, ok := capability.(map[string]any)
	if !ok {
		return modelport.BindingDescriptor{}, contractFailed(nil, "the harness-v1 advertisement is not an object")
	}
	if adv["normalization"] != "strict-v1" || adv["measure"] != true || adv["attempt_receipt"] != true ||
		adv["explicit_recovery"] != true || adv["generation_retry"] != "disabled" {
		return modelport.BindingDescriptor{}, unsupported("the harness-v1 advertisement is not the contract this client speaks")
	}
	list, ok := adv["bindings"].([]any)
	if !ok {
		return modelport.BindingDescriptor{}, contractFailed(nil, "the harness-v1 advertisement has no bindings list")
	}
	var match any
	for _, entry := range list {
		obj, _ := entry.(map[string]any)
		named, _ := obj["binding"].(map[string]any)
		if named["kind"] != b.Kind || named["selector"] != b.Selector {
			continue
		}
		if match != nil {
			return modelport.BindingDescriptor{}, contractFailed(nil, "the binding is described twice")
		}
		match = entry
	}
	if match == nil {
		return modelport.BindingDescriptor{}, unsupported("the binding is not listed as ready")
	}
	var desc modelport.BindingDescriptor
	if err := schemacheck.Unmarshal(schemacheck.LLMContract, "BindingDescriptor", match, &desc); err != nil {
		return modelport.BindingDescriptor{}, contractFailed(nil, "the binding descriptor is not valid")
	}
	if desc.BindingFingerprint == unverifiedFingerprint {
		return modelport.BindingDescriptor{}, contractFailed(nil, "the binding descriptor carries no fingerprint")
	}
	return desc, nil
}
