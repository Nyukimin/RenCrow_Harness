package modelclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewBuildsEndpointsFromTheBaseWithoutCorrecting(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"http://127.0.0.1:8080/v1", "http://127.0.0.1:8080/v1"},
		{"http://127.0.0.1:8080/v1/", "http://127.0.0.1:8080/v1"},
		{"http://localhost:8080/v1", "http://localhost:8080/v1"},
		{"https://localhost/v1/", "https://localhost/v1"},
		{"http://[::1]:9090/v1", "http://[::1]:9090/v1"},
		{"http://127.0.0.2:1/v1", "http://127.0.0.2:1/v1"},
		// A base that already has the version is not "fixed", and one without it is
		// not completed: the path is exactly what the configuration says.
		{"http://127.0.0.1:8080/v1/v1", "http://127.0.0.1:8080/v1/v1"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
	} {
		c, err := New(tc.base, Options{})
		if err != nil {
			t.Fatalf("%s: %v", tc.base, err)
		}
		if got := c.endpoint("/chat/completions"); got != tc.want+"/chat/completions" {
			t.Errorf("%s: endpoint %q", tc.base, got)
		}
		if got := c.endpoint("/context/measure"); got != tc.want+"/context/measure" {
			t.Errorf("%s: endpoint %q", tc.base, got)
		}
	}
}

func TestNewRefusesWhatIsNotAPlainLoopbackURL(t *testing.T) {
	for _, base := range []string{
		"", "127.0.0.1:8080/v1", "ftp://127.0.0.1/v1", "http:///v1", "http://example.com/v1", "http://10.0.0.5:8080/v1",
		"http://0.0.0.0:8080/v1", "http://192.168.1.2/v1", "http://[2001:db8::1]/v1", "http://localhost.example.com/v1",
		"http://user:pw@127.0.0.1:8080/v1", "http://127.0.0.1:8080/v1?x=1", "http://127.0.0.1:8080/v1?", "http://127.0.0.1:8080/v1#f",
		"http://127.0.0.1:8080/v1//", "http://127.0.0.1:8080//v1", "http://127.0.0.1:8080/v1/../v1", "http://127.0.0.1:8080/v%31",
		"mailto:127.0.0.1",
	} {
		c, err := New(base, Options{})
		if err == nil {
			t.Errorf("%q: accepted as %v", base, c.base)
			continue
		}
		// The refusal never repeats the URL, which may carry what must not be shown.
		if base != "" && strings.Contains(err.Error(), "pw") {
			t.Errorf("%q: the error repeats the URL: %v", base, err)
		}
	}
}

func TestLoopbackOnlyDialer(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:80": true, "[::1]:80": true, "127.9.9.9:1": true,
		"10.1.2.3:80": false, "8.8.8.8:443": false, "[2001:db8::1]:80": false, "0.0.0.0:80": false, "not-an-address": false, "example.com:80": false,
	} {
		err := loopbackOnly("tcp", addr, nil)
		if (err == nil) != ok {
			t.Errorf("%s: loopbackOnly = %v, want ok=%v", addr, err, ok)
		}
	}
}

func TestTransportNeverReusesFollowsOrProxies(t *testing.T) {
	g := newGateway(t)
	g.reply(statusKey, http.StatusOK, "application/json", []byte(`{}`))
	c := g.client()
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || !tr.DisableKeepAlives || tr.ForceAttemptHTTP2 {
		t.Fatalf("the transport is not the one-shot, proxy-less one: %+v", tr)
	}
	if _, err := c.Describe(t.Context(), testBinding); err == nil {
		t.Fatal("an empty status is not a description")
	}
	if got := g.header(statusKey, 0).Get("Connection"); got != "close" {
		t.Errorf("Connection: %q, want close", got)
	}
	if got := g.header(statusKey, 0).Get("User-Agent"); got != "rencrow-harness" {
		t.Errorf("User-Agent: %q", got)
	}
}

func TestARedirectIsAnAnswerAndIsNotFollowed(t *testing.T) {
	var elsewhere int
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere++ }))
	defer other.Close()
	g := newGateway(t)
	g.on(generateKey, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
	})
	req := genRequest(t, actRequest(t))
	rc, err := g.client().Generate(t.Context(), req)
	if rc != nil {
		t.Fatal("a redirect is not a stream")
	}
	se := asStrict(t, err)
	if se.Code != "MODEL_CONTRACT_FAILED" || se.Receipt != nil || stateOf(err) != "unknown" {
		t.Fatalf("a redirect is a contract failure with an unknown generation: %+v", se)
	}
	if elsewhere != 0 {
		t.Fatal("the request was sent to the redirect target")
	}
	if g.count(generateKey) != 1 {
		t.Fatalf("%d requests, want 1", g.count(generateKey))
	}
	if se.SourceCode == nil || *se.SourceCode != "HTTP_307" {
		t.Errorf("the status is kept as a diagnostic only: %v", se.SourceCode)
	}
}

func TestARedirectOfAReadIsNotFollowedEither(t *testing.T) {
	var elsewhere int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere++
		_, _ = w.Write([]byte(`{"contracts":{}}`))
	}))
	defer other.Close()
	g := newGateway(t)
	g.on(statusKey, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/status", http.StatusFound)
	})
	if _, err := g.client().Describe(t.Context(), testBinding); err == nil {
		t.Fatal("a redirect is not a description")
	}
	if elsewhere != 0 || g.count(statusKey) != 1 {
		t.Fatalf("the redirect was followed (%d), the Gateway was asked %d times", elsewhere, g.count(statusKey))
	}
}
