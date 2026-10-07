package cli_test

import (
	"strings"
	"testing"
)

// TestServeNeedsTheGatewayAndTheBindingsOfItsConfiguration: what the production composition
// is built from (the Gateway's address and contract, and the bindings a Run may use) is
// required by the configuration, and a configuration without it, or with it wrong, stops
// `serve` at startup before anything is served. Nothing is defaulted.
func TestServeNeedsTheGatewayAndTheBindingsOfItsConfiguration(t *testing.T) {
	for name, edit := range map[string]func(cfg map[string]any){
		"no gateway section":        func(cfg map[string]any) { delete(cfg, "gateway") },
		"no gateway address":        func(cfg map[string]any) { delete(cfg["gateway"].(map[string]any), "base_url") },
		"an empty gateway address":  func(cfg map[string]any) { cfg["gateway"].(map[string]any)["base_url"] = "" },
		"no required contract":      func(cfg map[string]any) { delete(cfg["gateway"].(map[string]any), "required_contract") },
		"another contract":          func(cfg map[string]any) { cfg["gateway"].(map[string]any)["required_contract"] = "harness-v2" },
		"no bindings":               func(cfg map[string]any) { delete(cfg, "bindings") },
		"an empty list of bindings": func(cfg map[string]any) { cfg["bindings"] = []any{} },
		"a binding that is not resolved": func(cfg map[string]any) {
			cfg["bindings"].([]any)[0].(map[string]any)["binding"].(map[string]any)["profile_revision"] = "latest"
		},
		"an unknown key in the gateway": func(cfg map[string]any) { cfg["gateway"].(map[string]any)["api_key"] = "secret-marker" },
		"a gateway address with a secret": func(cfg map[string]any) {
			cfg["gateway"].(map[string]any)["base_url"] = "http://127.0.0.1:8090/v1?key=secret-marker"
		},
	} {
		t.Run(name, func(t *testing.T) {
			l := deployment(t)
			edit(l.Cfg)
			l.Write()
			code, out, errs := run(t, "", "serve", "--stdio", "--config", l.Config)
			if code != 64 || out != "" || errs == "" || strings.Contains(errs, "secret-marker") {
				t.Fatalf("%d %q %q", code, out, errs)
			}
		})
	}
}
