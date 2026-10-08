package harnesstest

import (
	"net"
	"net/url"
	"testing"
	"time"
)

func gatewayPort(t *testing.T, cfg map[string]any) string {
	t.Helper()
	raw, _ := cfg["gateway"].(map[string]any)["base_url"].(string)
	u, err := url.Parse(raw)
	if err != nil || u.Port() == "" {
		t.Fatalf("the Gateway address %q has no port: %v", raw, err)
	}
	return u.Port()
}

// TestLayoutNamesAGatewayNothingListensOn: a deployment that has not been given a Gateway
// double names a loopback port that nothing listens on, and not the port of the design
// package's example configuration (the port a real Gateway serves on). A host that runs the
// real Gateway would otherwise answer a Run that is meant to find the Gateway down.
func TestLayoutNamesAGatewayNothingListensOn(t *testing.T) {
	examplePort := gatewayPort(t, example(t, "config.json"))
	for i := 0; i < 5; i++ {
		l := NewLayout(t, Options{})
		port := gatewayPort(t, l.Cfg)
		if port == examplePort {
			t.Fatalf("the deployment names the example Gateway port %s, where a real Gateway may be serving", port)
		}
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 2*time.Second)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("something listens on the Gateway port %s the deployment names", port)
		}
	}
}
