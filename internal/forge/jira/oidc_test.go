package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

	"github.com/fullsend-ai/fullsend/internal/inference/actionsoidc"
)

const (
	oidcTestAudience = "jira-gateway.example"
	oidcTestCloudID  = "11111111-2222-3333-4444-555555555555"
	// The httptest certificate covers *.example.com; the fixture's
	// transport dials every host to the one test server.
	oidcTestSiteURL    = "https://site.example.com"
	oidcTestGatewayURL = "https://jira-gw.example.com"
)

// oidcTestJWT returns an unsigned JWT with the given aud, iat and exp.
func oidcTestJWT(aud string, iat, exp time.Time, nonce string) string {
	enc := base64.RawURLEncoding.EncodeToString
	payload, _ := json.Marshal(map[string]any{"aud": aud, "iat": iat.Unix(), "exp": exp.Unix(), "jti": nonce})
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

func oidcTestAssertion(aud string, lifetime time.Duration, nonce string) *actionsoidc.Assertion {
	iat := time.Now().Add(-time.Second).Truncate(time.Second)
	exp := iat.Add(lifetime)
	return &actionsoidc.Assertion{Value: oidcTestJWT(aud, iat, exp, nonce), IssuedAt: iat, ExpiresAt: exp}
}

// oidcGateway is a fake self-hosted Jira gateway. It accepts a Bearer JWT
// whose aud is oidcTestAudience and whose exp is in the future, unless the
// token is listed in refused.
type oidcGateway struct {
	srv      *httptest.Server
	mu       sync.Mutex
	refused  map[string]bool
	tokens   []string
	searchSC int
	search   string
}

func newOIDCGateway(t *testing.T) *oidcGateway {
	t.Helper()
	g := &oidcGateway{refused: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/_edge/tenant_info", func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "site.example.com" || r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"cloudId":"` + oidcTestCloudID + `"}`))
	})
	api := "/ex/jira/" + oidcTestCloudID + "/rest/api/3"
	mux.HandleFunc(api+"/myself", func(w http.ResponseWriter, r *http.Request) {
		if !g.accept(w, r) {
			return
		}
		_, _ = w.Write([]byte(`{"accountId":"acct-1","active":true}`))
	})
	mux.HandleFunc(api+"/search/jql", func(w http.ResponseWriter, r *http.Request) {
		if !g.accept(w, r) {
			return
		}
		g.mu.Lock()
		sc, body := g.searchSC, g.search
		g.mu.Unlock()
		if sc != 0 {
			w.WriteHeader(sc)
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(`{"issues":[],"isLast":true}`))
	})
	g.srv = httptest.NewTLSServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

// accept checks the Bearer JWT and answers 401 when it is missing,
// expired, for another audience, or refused.
func (g *oidcGateway) accept(w http.ResponseWriter, r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	g.mu.Lock()
	g.tokens = append(g.tokens, tok)
	refused := g.refused[tok]
	g.mu.Unlock()
	if !ok || r.Host != "jira-gw.example.com" || refused || !oidcTokenValid(tok, oidcTestAudience) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid token"}`))
		return false
	}
	return true
}

// oidcTokenValid decodes the JWT payload and checks aud and exp.
func oidcTokenValid(tok, audience string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return false
	}
	return claims.Aud == audience && time.Now().Before(time.Unix(claims.Exp, 0))
}

func (g *oidcGateway) refuse(tok string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refused[tok] = true
}

func (g *oidcGateway) setSearch(status int, body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.searchSC, g.search = status, body
}

func (g *oidcGateway) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.tokens...)
}

// client returns an HTTP client that trusts the test certificate and
// dials every host to the test server.
func (g *oidcGateway) client() *http.Client {
	tr := g.srv.Client().Transport.(*http.Transport).Clone()
	addr := g.srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return &http.Client{Transport: tr, CheckRedirect: checkRedirect, Timeout: 10 * time.Second}
}

func oidcScopedConfig() ScopedConfig {
	return ScopedConfig{BaseURL: oidcTestSiteURL, APIURL: oidcTestGatewayURL, Auth: AuthOIDC, Audience: oidcTestAudience}
}

// countingFetch returns a fetch that issues a new assertion per call.
func countingFetch(aud string, lifetime time.Duration, calls *atomic.Int32) OIDCFetchFunc {
	return func(context.Context) (*actionsoidc.Assertion, error) {
		n := calls.Add(1)
		return oidcTestAssertion(aud, lifetime, string(rune('a'+n))), nil
	}
}

func TestNewScoped_OIDCSendsBearerToGateway(t *testing.T) {
	g := newOIDCGateway(t)
	var calls atomic.Int32
	c, err := NewScoped(oidcScopedConfig(), Credentials{}, WithHTTPClient(g.client()), WithOIDCFetch(countingFetch(oidcTestAudience, 5*time.Minute, &calls)))
	require.NoError(t, err)
	me, err := c.GetMyself(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "acct-1", me.AccountID)
	_, err = c.GetMyself(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int32(1), calls.Load(), "the cached token is reused until it nears exp")
	require.Len(t, g.seen(), 2)
	assert.True(t, oidcTokenValid(g.seen()[0], oidcTestAudience))
}

func TestNewScoped_OIDCRefetchesOnceOn401(t *testing.T) {
	g := newOIDCGateway(t)
	var calls atomic.Int32
	c, err := NewScoped(oidcScopedConfig(), Credentials{}, WithHTTPClient(g.client()), WithOIDCFetch(countingFetch(oidcTestAudience, 5*time.Minute, &calls)))
	require.NoError(t, err)
	_, err = c.GetMyself(context.Background())
	require.NoError(t, err)
	// The gateway now refuses the cached token: one refetch, then success.
	g.refuse(g.seen()[0])
	_, err = c.GetMyself(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int32(2), calls.Load())
}

func TestNewScoped_OIDCFailsWhenFreshTokenRefused(t *testing.T) {
	g := newOIDCGateway(t)
	var calls atomic.Int32
	// Every token is for the wrong audience, so the gateway refuses all.
	c, err := NewScoped(oidcScopedConfig(), Credentials{}, WithHTTPClient(g.client()), WithOIDCFetch(countingFetch("other-audience", 5*time.Minute, &calls)))
	require.NoError(t, err)
	_, err = c.GetMyself(context.Background())
	require.Error(t, err)
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusUnauthorized, apiErr.StatusCode)
	assert.Contains(t, err.Error(), "refused the OIDC token, also after fetching a fresh one")
	assert.Equal(t, int32(2), calls.Load(), "exactly one refetch")
	for _, tok := range g.seen() {
		assert.NotContains(t, err.Error(), tok)
	}
}

func TestNewScoped_OIDCSameTokenAfter401Fails(t *testing.T) {
	g := newOIDCGateway(t)
	a := oidcTestAssertion(oidcTestAudience, 5*time.Minute, "fixed")
	g.refuse(a.Value)
	c, err := NewScoped(oidcScopedConfig(), Credentials{}, WithHTTPClient(g.client()),
		WithOIDCFetch(func(context.Context) (*actionsoidc.Assertion, error) { return a, nil }))
	require.NoError(t, err)
	_, err = c.GetMyself(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no fresh token can be fetched")
	assert.NotContains(t, err.Error(), a.Value)
}

func TestNewScoped_OIDC403SanitizesGatewayError(t *testing.T) {
	g := newOIDCGateway(t)
	var calls atomic.Int32
	c, err := NewScoped(oidcScopedConfig(), Credentials{}, WithHTTPClient(g.client()), WithOIDCFetch(countingFetch(oidcTestAudience, 5*time.Minute, &calls)))
	require.NoError(t, err)

	g.setSearch(http.StatusForbidden, `{"error":"query not allowed"}`)
	_, err = c.SearchIssues(context.Background(), "project = ABC", 10)
	require.Error(t, err)
	assert.ErrorIs(t, err, errForbiddenSentinel())
	assert.Contains(t, err.Error(), "query not allowed")

	g.setSearch(http.StatusForbidden, "{\"error\":\"line1\\n::error::injected\"}")
	_, err = c.SearchIssues(context.Background(), "project = ABC", 10)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "injected", "an unsafe gateway message is not quoted")
	assert.Contains(t, err.Error(), "Forbidden")
}

func errForbiddenSentinel() error {
	return (&APIError{StatusCode: http.StatusForbidden}).Unwrap()
}

func TestNewScoped_OIDCRefusesCredentialsAndNonGatewayURLs(t *testing.T) {
	g := newOIDCGateway(t)
	fetch := WithOIDCFetch(func(context.Context) (*actionsoidc.Assertion, error) {
		return oidcTestAssertion(oidcTestAudience, time.Minute, "x"), nil
	})
	for name, tc := range map[string]struct {
		mutate  func(*ScopedConfig)
		creds   Credentials
		opts    []Option
		wantErr string
	}{
		"with a token":          {creds: Credentials{Token: "secret-token"}, opts: []Option{fetch}, wantErr: "no Jira credential"},
		"with client creds":     {creds: Credentials{ClientID: "id", ClientSecret: "secret"}, opts: []Option{fetch}, wantErr: "no Jira credential"},
		"no audience":           {mutate: func(c *ScopedConfig) { c.Audience = " " }, opts: []Option{fetch}, wantErr: "requires tracker.jira.audience"},
		"no api_url":            {mutate: func(c *ScopedConfig) { c.APIURL = "" }, opts: []Option{fetch}, wantErr: "requires tracker.jira.api_url"},
		"http api_url":          {mutate: func(c *ScopedConfig) { c.APIURL = "http://jira-gw.example.com" }, opts: []Option{fetch}, wantErr: "https"},
		"loopback http api_url": {mutate: func(c *ScopedConfig) { c.APIURL = "http://127.0.0.1:9" }, opts: []Option{fetch}, wantErr: "https"},
		"api.atlassian.com":     {mutate: func(c *ScopedConfig) { c.APIURL = "https://API.atlassian.com" }, opts: []Option{fetch}, wantErr: "Atlassian host"},
		"trailing dot":          {mutate: func(c *ScopedConfig) { c.APIURL = "https://API.atlassian.com." }, opts: []Option{fetch}, wantErr: "ASCII DNS name"},
		"site host":             {mutate: func(c *ScopedConfig) { c.APIURL = "https://site.example.com/gw" }, opts: []Option{fetch}, wantErr: "Jira site host"},
		"Unicode-dot (U+3002) Atlassian host is refused": {mutate: func(c *ScopedConfig) { c.APIURL = "https://api.atlassian\u3002com" }, opts: []Option{fetch}, wantErr: "ASCII DNS name"},
		"Unicode-dot (U+FF0E) Atlassian host is refused": {mutate: func(c *ScopedConfig) { c.APIURL = "https://api.atlassian\uff0ecom" }, opts: []Option{fetch}, wantErr: "ASCII DNS name"},
		"Unicode-dot (U+FF61) Atlassian host is refused": {mutate: func(c *ScopedConfig) { c.APIURL = "https://api.atlassian\uff61com" }, opts: []Option{fetch}, wantErr: "ASCII DNS name"},
		"atlassian.net":          {mutate: func(c *ScopedConfig) { c.APIURL = "https://x.atlassian.net" }, opts: []Option{fetch}, wantErr: "Atlassian host"},
		"unknown auth":           {mutate: func(c *ScopedConfig) { c.Auth = "jwt" }, wantErr: "not supported"},
		"no OIDC endpoint":       {wantErr: "id-token: write"},
		"endpoint token missing": {mutate: func(c *ScopedConfig) { c.OIDCRequestURL = "https://x.actions.githubusercontent.com" }, wantErr: "id-token: write"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := oidcScopedConfig()
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			_, err := NewScoped(cfg, tc.creds, append([]Option{WithHTTPClient(g.client())}, tc.opts...)...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestNewScoped_OIDCDefaultFetchUsesRunnerEndpoint(t *testing.T) {
	g := newOIDCGateway(t)
	oidcSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"value": oidcTestAssertion(r.URL.Query().Get("audience"), 5*time.Minute, "rt").Value})
	}))
	t.Cleanup(oidcSrv.Close)
	cfg := oidcScopedConfig()
	cfg.OIDCRequestURL, cfg.OIDCRequestToken = oidcSrv.URL+"/token?api-version=2.0", "request-token"
	c, err := NewScoped(cfg, Credentials{}, WithHTTPClient(g.client()))
	require.NoError(t, err)
	// The default fetch uses actionsoidc's own client, which does not
	// trust the test certificate: the failure proves the endpoint and
	// audience are wired and the token never reaches Atlassian.
	_, err = c.GetMyself(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fetching the OIDC token for the Jira gateway")
	assert.Contains(t, err.Error(), oidcTestAudience)
	assert.Empty(t, g.seen(), "no request reached the gateway without a token")
}

func TestOIDCTokenSource_RefreshFromExpAndIat(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var calls atomic.Int32
	var lifetime time.Duration
	s := &oidcTokenSource{
		audience: oidcTestAudience,
		sem:      make(chan struct{}, 1),
		now:      func() time.Time { return now },
		fetch: func(context.Context) (*actionsoidc.Assertion, error) {
			n := calls.Add(1)
			iat := now
			return &actionsoidc.Assertion{Value: oidcTestJWT(oidcTestAudience, iat, iat.Add(lifetime), string(rune('a'+n))), IssuedAt: iat, ExpiresAt: iat.Add(lifetime)}, nil
		},
	}
	ctx := context.Background()

	// 300 s token: skew is min(60 s, 75 s) = 60 s, so refetch at 240 s.
	lifetime = 300 * time.Second
	first, err := s.token(ctx, "")
	require.NoError(t, err)
	now = now.Add(239 * time.Second)
	again, err := s.token(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, first, again)
	now = now.Add(time.Second)
	second, err := s.token(ctx, "")
	require.NoError(t, err)
	assert.NotEqual(t, first, second)
	assert.Equal(t, int32(2), calls.Load())

	// 120 s token: skew is lifetime/4 = 30 s, so refetch at 90 s.
	lifetime = 120 * time.Second
	s.value = ""
	third, err := s.token(ctx, "")
	require.NoError(t, err)
	now = now.Add(89 * time.Second)
	got, err := s.token(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, third, got)
	now = now.Add(time.Second)
	got, err = s.token(ctx, "")
	require.NoError(t, err)
	assert.NotEqual(t, third, got)

	// A stale token is replaced even before its refresh time.
	fresh, err := s.token(ctx, got)
	require.NoError(t, err)
	assert.NotEqual(t, got, fresh)
}

func TestOIDCTokenSource_RefusesUnusableTokens(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	mk := func(fetch OIDCFetchFunc) *oidcTokenSource {
		return &oidcTokenSource{audience: "aud", sem: make(chan struct{}, 1), now: func() time.Time { return now }, fetch: fetch}
	}
	for name, tc := range map[string]struct {
		a       *actionsoidc.Assertion
		err     error
		wantErr string
	}{
		"fetch error":  {err: errors.New("boom"), wantErr: "boom"},
		"nil":          {wantErr: "returned no token"},
		"control char": {a: &actionsoidc.Assertion{Value: "a.b\n.c", IssuedAt: now, ExpiresAt: now.Add(time.Minute)}, wantErr: "printable ASCII"},
		"exp<=iat":     {a: &actionsoidc.Assertion{Value: "a.b.c", IssuedAt: now, ExpiresAt: now}, wantErr: "exp is not after"},
		"expired":      {a: &actionsoidc.Assertion{Value: "a.b.c", IssuedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute)}, wantErr: "expired at"},
	} {
		t.Run(name, func(t *testing.T) {
			s := mk(func(context.Context) (*actionsoidc.Assertion, error) { return tc.a, tc.err })
			_, err := s.token(context.Background(), "")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := mk(nil)
	s.sem <- struct{}{}
	_, err := s.token(ctx, "")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestCheckNoCredentialEnv(t *testing.T) {
	require.NoError(t, CheckNoCredentialEnv(func(string) string { return "" }))
	env := map[string]string{TokenEnvVar: "tok-value", ClientSecretEnvVar: "sec-value"}
	err := CheckNoCredentialEnv(func(k string) string { return env[k] })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FULLSEND_JIRA_TOKEN, FULLSEND_JIRA_CLIENT_SECRET are set")
	assert.NotContains(t, err.Error(), "value")
	err = CheckNoCredentialEnv(func(k string) string {
		if k == EmailEnvVar {
			return "bot@example.com"
		}
		return ""
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FULLSEND_JIRA_EMAIL is set")
}

func TestNewScopedFromEnv_OIDCRefusesSecretsGitLabAndMissingEndpoint(t *testing.T) {
	g := newOIDCGateway(t)
	var calls atomic.Int32
	opts := []Option{WithHTTPClient(g.client()), WithOIDCFetch(countingFetch(oidcTestAudience, 5*time.Minute, &calls))}

	c, err := NewScopedFromEnv(oidcScopedConfig(), func(string) string { return "" }, opts...)
	require.NoError(t, err)
	_, err = c.GetMyself(context.Background())
	require.NoError(t, err)

	_, err = NewScopedFromEnv(oidcScopedConfig(), func(k string) string {
		if k == ClientIDEnvVar {
			return "id"
		}
		return ""
	}, opts...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FULLSEND_JIRA_CLIENT_ID is set")

	_, err = NewScopedFromEnv(oidcScopedConfig(), func(k string) string {
		if k == "GITLAB_CI" {
			return "true"
		}
		return ""
	}, opts...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported on GitLab CI yet")

	_, err = NewScopedFromEnv(oidcScopedConfig(), func(string) string { return "" }, WithHTTPClient(g.client()))
	require.ErrorIs(t, err, ErrOIDCEndpointMissing)

	// Outside oidc mode the FULLSEND_JIRA_* secrets are still used.
	cfg := oidcScopedConfig()
	cfg.Auth, cfg.Audience = "", ""
	_, err = NewScopedFromEnv(cfg, func(string) string { return "" })
	require.Error(t, err)
	assert.Contains(t, err.Error(), TokenEnvVar)
}
