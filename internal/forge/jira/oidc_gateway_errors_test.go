package jira

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingGateway is a TLS server for site.example.com and
// jira-gw.example.com that records every request and answers the gateway's
// /myself with the status and body set by the test.
type recordingGateway struct {
	srv    *httptest.Server
	mu     sync.Mutex
	reqs   []string // "host path auth"
	status atomic.Int32
	body   atomic.Value // string
	// okAfter, when > 0, answers 200 from that request number on.
	okAfter atomic.Int32
	myself  atomic.Int32
}

func newRecordingGateway(t *testing.T) *recordingGateway {
	t.Helper()
	g := &recordingGateway{}
	g.body.Store("")
	g.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.reqs = append(g.reqs, r.Host+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		g.mu.Unlock()
		switch {
		case r.URL.Path == "/_edge/tenant_info":
			_, _ = w.Write([]byte(`{"cloudId":"` + oidcTestCloudID + `"}`))
		case strings.HasSuffix(r.URL.Path, "/rest/api/3/myself"):
			n := g.myself.Add(1)
			if after := g.okAfter.Load(); after > 0 && n >= after {
				_, _ = w.Write([]byte(`{"accountId":"acct-1"}`))
				return
			}
			w.WriteHeader(int(g.status.Load()))
			_, _ = w.Write([]byte(g.body.Load().(string)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *recordingGateway) requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.reqs...)
}

func (g *recordingGateway) newClient(t *testing.T) *LiveClient {
	t.Helper()
	tr := g.srv.Client().Transport.(*http.Transport).Clone()
	addr := g.srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	var calls atomic.Int32
	c, err := NewScoped(oidcScopedConfig(), Credentials{},
		WithHTTPClient(&http.Client{Transport: tr, CheckRedirect: checkRedirect, Timeout: 30 * time.Second}),
		WithOIDCFetch(countingFetch(oidcTestAudience, 5*time.Minute, &calls)))
	require.NoError(t, err)
	return c
}

func TestOIDC_GatewayUpstreamAuthIsNotRetried(t *testing.T) {
	g := newRecordingGateway(t)
	g.status.Store(http.StatusBadGateway)
	g.body.Store(`{"error":"Jira refused the gateway credential","code":"upstream_auth"}`)
	_, err := g.newClient(t).GetMyself(context.Background())
	require.Error(t, err)
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, GatewayCodeUpstreamAuth, apiErr.GatewayCode)
	assert.Contains(t, err.Error(), "gateway: Jira refused the gateway credential (upstream_auth)", "the gateway's text is shown")
	assert.Equal(t, int32(1), g.myself.Load(), "exactly one request")
}

func TestAPIError_UpstreamAuthIsNotTransient(t *testing.T) {
	assert.False(t, (&APIError{StatusCode: http.StatusBadGateway, GatewayCode: GatewayCodeUpstreamAuth}).IsTransient())
	assert.True(t, (&APIError{StatusCode: http.StatusBadGateway, GatewayCode: "upstream_error"}).IsTransient())
	assert.True(t, (&APIError{StatusCode: http.StatusBadGateway}).IsTransient())
}

func TestOIDC_TokenHookSeesEveryFetchedToken(t *testing.T) {
	g := newOIDCGateway(t)
	var calls atomic.Int32
	var mu sync.Mutex
	var hooked []string
	c, err := NewScoped(oidcScopedConfig(), Credentials{}, WithHTTPClient(g.client()),
		WithOIDCFetch(countingFetch(oidcTestAudience, 5*time.Minute, &calls)),
		WithOIDCTokenHook(func(tok string) { mu.Lock(); hooked = append(hooked, tok); mu.Unlock() }))
	require.NoError(t, err)
	_, err = c.GetMyself(context.Background())
	require.NoError(t, err)
	g.refuse(g.seen()[0])
	_, err = c.GetMyself(context.Background())
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, hooked, 2, "the first token and the refetch after a 401")
	assert.Equal(t, g.seen()[0], hooked[0])
}

func TestOIDC_GatewayUpstreamErrorIsRetried(t *testing.T) {
	g := newRecordingGateway(t)
	g.status.Store(http.StatusBadGateway)
	g.body.Store(`{"error":"Jira answered 500","code":"upstream_error"}`)
	g.okAfter.Store(2)
	me, err := g.newClient(t).GetMyself(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "acct-1", me.AccountID)
	assert.Equal(t, int32(2), g.myself.Load(), "a 502 without upstream_auth keeps today's retry")
}

func TestOIDC_GatewayErrorBodiesAreSanitized(t *testing.T) {
	for name, tc := range map[string]struct {
		body     string
		wantMsg  string
		wantCode string
		absent   string
	}{
		"text and code":    {body: `{"error":"endpoint not allowed","code":"endpoint_not_allowed"}`, wantMsg: "gateway: endpoint not allowed (endpoint_not_allowed)", wantCode: "endpoint_not_allowed"},
		"text only":        {body: `{"error":"not granted"}`, wantMsg: "jira api: 403 not granted", absent: "gateway:"},
		"bad code dropped": {body: `{"error":"x","code":"Bad-Code!"}`, wantMsg: "jira api: 403 x", absent: "Bad-Code"},
		"jira int code":    {body: `{"errorMessages":["Issue does not exist"],"code":401}`, wantMsg: "Issue does not exist", absent: "gateway"},
		"control chars":    {body: "{\"error\":\"a\\n::error::injected\",\"code\":\"not_granted\"}", wantMsg: "Forbidden (gateway code not_granted)", wantCode: "not_granted", absent: "injected"},
		"oversized text":   {body: `{"error":"` + strings.Repeat("A", 201) + `","code":"not_granted"}`, wantMsg: "gateway code not_granted", wantCode: "not_granted", absent: strings.Repeat("A", 201)},
		"not json":         {body: `<html>secret page</html>`, wantMsg: "Forbidden", absent: "secret"},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRecordingGateway(t)
			g.status.Store(http.StatusForbidden)
			g.body.Store(tc.body)
			_, err := g.newClient(t).GetMyself(context.Background())
			require.Error(t, err)
			var apiErr *APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Contains(t, err.Error(), tc.wantMsg)
			assert.Equal(t, tc.wantCode, apiErr.GatewayCode)
			if tc.absent != "" {
				assert.NotContains(t, err.Error(), tc.absent)
			}
		})
	}
}

// TestOIDC_TenantInfoHasNoCredentialAndNoOAuthTokenRequestIsMade: cloud ID discovery carries no
// Authorization header, every gateway request carries the OIDC Bearer,
// and nothing ever goes to the Atlassian OAuth token endpoint.
func TestOIDC_TenantInfoHasNoCredentialAndNoOAuthTokenRequestIsMade(t *testing.T) {
	g := newRecordingGateway(t)
	g.okAfter.Store(1)
	c := g.newClient(t)
	assert.Nil(t, c.oauth, "no OAuth token source in oidc mode")
	_, err := c.GetMyself(context.Background())
	require.NoError(t, err)
	reqs := g.requests()
	require.Len(t, reqs, 2)
	assert.Equal(t, "site.example.com /_edge/tenant_info ", reqs[0], "tenant_info carries no Authorization")
	assert.True(t, strings.HasPrefix(reqs[1], "jira-gw.example.com /ex/jira/"+oidcTestCloudID+"/rest/api/3/myself Bearer "))
	for _, r := range reqs {
		assert.NotContains(t, r, "/oauth/token")
		assert.NotContains(t, r, "auth.atlassian.com")
	}
}
