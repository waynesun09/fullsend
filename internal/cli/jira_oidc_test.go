package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/config"
	"github.com/fullsend-ai/fullsend/internal/forge/jira"
	"github.com/fullsend-ai/fullsend/internal/harness"
	"github.com/fullsend-ai/fullsend/internal/inference/actionsoidc"
	"github.com/fullsend-ai/fullsend/internal/security"
	"github.com/fullsend-ai/fullsend/internal/tracker"
	"github.com/fullsend-ai/fullsend/internal/ui"
)

const (
	oidcJiraAudience = "jira-gateway.example"
	// The httptest certificate covers *.example.com; the fixture's client
	// dials every host to the one test server.
	oidcJiraSite    = "https://site.example.com"
	oidcJiraGateway = "https://jira-gw.example.com"
	oidcJiraConfig  = "tracker:\n  jira:\n    base_url: " + oidcJiraSite + "\n    api_url: " + oidcJiraGateway +
		"\n    auth: oidc\n    audience: " + oidcJiraAudience + "\n    project: ABC\n"
)

// testOIDCJWT returns an unsigned JWT with the given aud, iat and exp.
func testOIDCJWT(aud string, iat, exp time.Time, nonce string) string {
	enc := base64.RawURLEncoding.EncodeToString
	payload, _ := json.Marshal(map[string]any{"aud": aud, "iat": iat.Unix(), "exp": exp.Unix(), "jti": nonce})
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

func testOIDCAssertion(aud string, lifetime time.Duration, nonce string) *actionsoidc.Assertion {
	iat := time.Now().Add(-time.Second).Truncate(time.Second)
	exp := iat.Add(lifetime)
	return &actionsoidc.Assertion{Value: testOIDCJWT(aud, iat, exp, nonce), IssuedAt: iat, ExpiresAt: exp}
}

// fakeJiraGateway is a self-hosted Jira gateway that accepts a Bearer JWT
// for oidcJiraAudience whose exp is in the future, unless it is refused.
type fakeJiraGateway struct {
	srv      *httptest.Server
	mu       sync.Mutex
	tokens   []string
	refused  map[string]bool
	searchSC int
}

func newFakeJiraGateway(t *testing.T) *fakeJiraGateway {
	t.Helper()
	g := &fakeJiraGateway{refused: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/_edge/tenant_info", func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "site.example.com" || r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"cloudId":"` + testJiraCloudID + `"}`))
	})
	api := "/ex/jira/" + testJiraCloudID + "/rest/api/3"
	mux.HandleFunc(api+"/myself", func(w http.ResponseWriter, r *http.Request) {
		if g.accept(w, r) {
			_, _ = w.Write([]byte(`{"accountId":"acct-1","active":true}`))
		}
	})
	mux.HandleFunc(api+"/search/jql", func(w http.ResponseWriter, r *http.Request) {
		if !g.accept(w, r) {
			return
		}
		g.mu.Lock()
		sc := g.searchSC
		g.mu.Unlock()
		if sc != 0 {
			w.WriteHeader(sc)
			_, _ = w.Write([]byte(`{"error":"jql uses a forbidden field","code":"jql_grammar"}`))
			return
		}
		_, _ = w.Write([]byte(`{"issues":[],"isLast":true}`))
	})
	g.srv = httptest.NewTLSServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeJiraGateway) accept(w http.ResponseWriter, r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	g.mu.Lock()
	g.tokens = append(g.tokens, tok)
	refused := g.refused[tok]
	g.mu.Unlock()
	if !ok || r.Host != "jira-gw.example.com" || refused || !testOIDCTokenValid(tok, oidcJiraAudience) {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

func testOIDCTokenValid(tok, audience string) bool {
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
	return json.Unmarshal(raw, &claims) == nil && claims.Aud == audience && time.Now().Before(time.Unix(claims.Exp, 0))
}

func (g *fakeJiraGateway) seen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.tokens...)
}

func (g *fakeJiraGateway) refuse(tok string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refused[tok] = true
}

func (g *fakeJiraGateway) client() *http.Client {
	tr := g.srv.Client().Transport.(*http.Transport).Clone()
	addr := g.srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

// oidcFetchCounting issues a new assertion for aud on every call.
func oidcFetchCounting(aud string, lifetime time.Duration, calls *atomic.Int32) jira.OIDCFetchFunc {
	return func(context.Context) (*actionsoidc.Assertion, error) {
		n := calls.Add(1)
		return testOIDCAssertion(aud, lifetime, string(rune('a'+n))), nil
	}
}

// clearJiraSecrets unsets every FULLSEND_JIRA_* secret and the runner's
// OIDC endpoint for the test.
func clearJiraSecrets(t *testing.T) {
	t.Helper()
	for _, k := range []string{jira.TokenEnvVar, jira.EmailEnvVar, jira.ClientIDEnvVar, jira.ClientSecretEnvVar,
		jira.OIDCRequestURLEnvVar, jira.OIDCRequestTokenEnvVar, "GITLAB_CI"} {
		t.Setenv(k, "")
	}
}

func setJiraScopedTestOptions(t *testing.T, opts ...jira.Option) {
	t.Helper()
	prev := jiraScopedTestOptions
	jiraScopedTestOptions = opts
	t.Cleanup(func() { jiraScopedTestOptions = prev })
}

func TestNewJiraTrackerClient_OIDC(t *testing.T) {
	clearJiraSecrets(t)
	g := newFakeJiraGateway(t)
	var calls atomic.Int32
	setJiraScopedTestOptions(t, jira.WithHTTPClient(g.client()), jira.WithOIDCFetch(oidcFetchCounting(oidcJiraAudience, 5*time.Minute, &calls)))

	// `fullsend issues`, the status writer and the orphaned-status
	// finalizer all build their client here.
	for name, build := range map[string]func() (tracker.Client, error){
		"issues": func() (tracker.Client, error) {
			return newConfiguredTrackerClient(trackerJira, "", writeJiraPollConfig(t, oidcJiraConfig), nil)
		},
		"run status": func() (tracker.Client, error) {
			return newJiraTrackerClientFromConfig(parseTestConfig(t, oidcJiraConfig).IssueTracking())
		},
	} {
		t.Run(name, func(t *testing.T) {
			tc, err := build()
			require.NoError(t, err)
			user, err := tc.(*tracker.JiraClient).AuthenticatedUser(context.Background())
			require.NoError(t, err)
			assert.NotEmpty(t, user)
			seen := g.seen()
			require.NotEmpty(t, seen)
			assert.True(t, testOIDCTokenValid(seen[len(seen)-1], oidcJiraAudience), "the gateway gets a Bearer JWT for the configured audience")
		})
	}

	t.Run("refetches once when the gateway refuses the cached token", func(t *testing.T) {
		tc, err := newJiraTrackerClientFromConfig(parseTestConfig(t, oidcJiraConfig).IssueTracking())
		require.NoError(t, err)
		jc := tc.(*tracker.JiraClient)
		_, err = jc.AuthenticatedUser(context.Background())
		require.NoError(t, err)
		before := calls.Load()
		seen := g.seen()
		g.refuse(seen[len(seen)-1])
		_, err = jc.AuthenticatedUser(context.Background())
		require.NoError(t, err)
		assert.Equal(t, before+1, calls.Load())
	})
}

func TestNewJiraTrackerClient_OIDCRefusesLeftoverSecretsMissingIDTokenAndGitLab(t *testing.T) {
	g := newFakeJiraGateway(t)
	cfg := parseTestConfig(t, oidcJiraConfig).IssueTracking()

	t.Run("expired token is refused and the fresh one too", func(t *testing.T) {
		clearJiraSecrets(t)
		setJiraScopedTestOptions(t, jira.WithHTTPClient(g.client()), jira.WithOIDCFetch(func(context.Context) (*actionsoidc.Assertion, error) {
			// Valid by its own claims for the client, but for another
			// audience, so the gateway refuses every token.
			return testOIDCAssertion("other", time.Minute, "x"), nil
		}))
		tc, err := newJiraTrackerClientFromConfig(cfg)
		require.NoError(t, err)
		_, err = tc.(*tracker.JiraClient).AuthenticatedUser(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "refused the OIDC token")
	})
	for _, k := range []string{jira.TokenEnvVar, jira.EmailEnvVar, jira.ClientIDEnvVar, jira.ClientSecretEnvVar} {
		t.Run("refused next to "+k, func(t *testing.T) {
			clearJiraSecrets(t)
			setJiraScopedTestOptions(t, jira.WithHTTPClient(g.client()))
			t.Setenv(k, "leftover-secret-value")
			_, err := newJiraTrackerClientFromConfig(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), k)
			assert.NotContains(t, err.Error(), "leftover-secret-value")
		})
	}
	t.Run("no id-token permission", func(t *testing.T) {
		clearJiraSecrets(t)
		setJiraScopedTestOptions(t, jira.WithHTTPClient(g.client()))
		_, err := newJiraTrackerClientFromConfig(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "id-token: write")
	})
	t.Run("gitlab", func(t *testing.T) {
		clearJiraSecrets(t)
		t.Setenv("GITLAB_CI", "true")
		_, err := newJiraTrackerClientFromConfig(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not supported on GitLab CI yet")
	})
}

func TestBuildJiraClient_OIDCPollerExplains403(t *testing.T) {
	clearJiraSecrets(t)
	g := newFakeJiraGateway(t)
	var calls atomic.Int32
	reader := parseTestConfig(t, oidcJiraConfig)
	j := reader.IssueTracking().Jira
	settings := jiraPollSettings{baseURL: j.BaseURL, apiURL: j.EffectiveAPIURL(), auth: j.Auth, audience: j.Audience, project: j.Project, jql: j.PollJQL()}

	c, err := buildJiraClient(settings, os.Getenv, jira.WithHTTPClient(g.client()), jira.WithOIDCFetch(oidcFetchCounting(oidcJiraAudience, 5*time.Minute, &calls)))
	require.NoError(t, err)
	pc := jiraPollClient(c, settings)
	_, err = pc.SearchIssues(context.Background(), settings.jql, 10)
	require.NoError(t, err, "the poller's search reaches the gateway with the OIDC token")

	g.mu.Lock()
	g.searchSC = http.StatusForbidden
	g.mu.Unlock()
	_, err = pc.SearchIssues(context.Background(), settings.jql, 10)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tracker.jira.poll.query")
	assert.Contains(t, err.Error(), "gateway refused the poll query")
	assert.Contains(t, err.Error(), "gateway: jql uses a forbidden field (jql_grammar)", "the gateway's text and code")

	// Outside oidc mode the client is used as it is.
	settings.auth = ""
	assert.Same(t, c, jiraPollClient(c, settings))

	t.Setenv(jira.TokenEnvVar, "leftover")
	settings.auth = jira.AuthOIDC
	_, err = buildJiraClient(settings, os.Getenv)
	require.Error(t, err)
	assert.Contains(t, err.Error(), jira.TokenEnvVar)
}

func TestLoadJiraPollSettings_OIDC(t *testing.T) {
	settings, err := loadJiraPollSettings(writeJiraPollConfig(t, oidcJiraConfig), "", "", "")
	require.NoError(t, err)
	assert.Equal(t, jira.AuthOIDC, settings.auth)
	assert.Equal(t, oidcJiraAudience, settings.audience)
	assert.Equal(t, oidcJiraGateway, settings.apiURL)
}

func oidcTracking(t *testing.T) *config.TrackingConfig {
	t.Helper()
	return parseTestConfig(t, oidcJiraConfig).IssueTracking()
}

func TestJiraProviderMints_OIDCRefusesStaticTokenAndASecondJiraProvider(t *testing.T) {
	unsetJiraToken(t)
	t.Setenv("JIRA_TOKEN_STATIC", "static-jira-token-value")
	def := func(v string) harness.ProviderDef {
		return harness.ProviderDef{Name: "jira", Type: jiraProviderType, Credentials: map[string]string{jiraCredentialKey: v}}
	}
	_, mint, err := jiraProviderMints(def("${FULLSEND_JIRA_TOKEN}"), true, jiraEnv(nil))
	require.NoError(t, err)
	assert.True(t, mint, "an empty JIRA_TOKEN carries the OIDC token")

	_, _, err = jiraProviderMints(def("${JIRA_TOKEN_STATIC}"), true, jiraEnv(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not expand to a static token")
	assert.NotContains(t, err.Error(), "static-jira-token-value")

	_, mint, err = jiraProviderMints(harness.ProviderDef{Name: "x", Credentials: map[string]string{"OTHER": ""}}, true, jiraEnv(nil))
	require.NoError(t, err)
	assert.False(t, mint)

	// Two JIRA_TOKEN providers: the one OIDC provider is refused company.
	ro := harness.ProviderDef{Name: "jira-ro", Type: "fullsend-jira-ro", Credentials: map[string]string{jiraCredentialKey: ""}}
	assert.True(t, jiraRunMints([]harness.ProviderDef{def("")}, true, jiraEnv(nil)))
	err = rejectMultipleMintedJiraProviders([]harness.ProviderDef{def(""), ro}, true, jiraEnv(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list only the")
}

func TestJiraOIDCRunPreflight(t *testing.T) {
	require.NoError(t, jiraOIDCRunPreflight(jiraEnv(nil)))
	err := jiraOIDCRunPreflight(jiraEnv(map[string]string{jira.ClientSecretEnvVar: "s3cr3t"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), jira.ClientSecretEnvVar)
	assert.NotContains(t, err.Error(), "s3cr3t")
	err = jiraOIDCRunPreflight(jiraEnv(map[string]string{"GITLAB_CI": "true"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported on GitLab CI yet")
}

func TestParseJiraOIDCGateway_RefusesNonHTTPSOtherPortsAndUnsafeHosts(t *testing.T) {
	gw, err := parseJiraOIDCGateway("https://Jira-GW.example.com/")
	require.NoError(t, err)
	assert.Equal(t, jiraOIDCGateway{host: "jira-gw.example.com"}, gw)
	gw, err = parseJiraOIDCGateway("https://jira-gw.example.com:443/jira/v1")
	require.NoError(t, err)
	assert.Equal(t, jiraOIDCGateway{host: "jira-gw.example.com", prefix: "/jira/v1"}, gw)
	for raw, want := range map[string]string{
		"http://jira-gw.example.com":       "https URL",
		"https://jira-gw.example.com:8443": "port 8443",
		"https://u:p@jira-gw.example.com":  "https URL",
		"https://jira-gw.example.com/a*b":  "path with characters",
		"https://jira_gw.example.com":      "not valid in a host name",
		"https://jira-gw.example.com?q=1":  "https URL",
	} {
		_, err := parseJiraOIDCGateway(raw)
		require.Error(t, err, raw)
		assert.Contains(t, err.Error(), want, raw)
		assert.NotContains(t, err.Error(), "u:p", raw)
	}
}

// TestRenderJiraSiteProfile_OIDCBindsOnlyTheGatewayHostAndSitePath: with a self-hosted gateway the
// per-site profile binds only the gateway host, with the site's path, so
// the OIDC token can never be injected on an Atlassian host.
func TestRenderJiraSiteProfile_OIDCBindsOnlyTheGatewayHostAndSitePath(t *testing.T) {
	gw := jiraOIDCGateway{host: "jira-gw.example.com", prefix: "/jira"}
	data, id, err := renderJiraSiteProfileFor(testJiraCloudID, &gw)
	require.NoError(t, err)
	assert.True(t, jiraSiteProfileIDPattern.MatchString(id), "the id stays reserved")
	assert.NotEqual(t, jiraSiteProfileID(testJiraCloudID), id, "a gateway profile never replaces the api.atlassian.com one")
	other, _, err := renderJiraSiteProfileFor(testJiraCloudID, &jiraOIDCGateway{host: "other.example.com"})
	require.NoError(t, err)
	assert.NotEqual(t, data, other)

	p := parseRenderedJiraProfile(t, data)
	assert.Equal(t, id, p.ID)
	require.Len(t, p.Endpoints, 1, "the *.atlassian.net site endpoint is dropped")
	assert.Equal(t, "jira-gw.example.com", p.Endpoints[0].Host)
	assert.Equal(t, "/jira/ex/jira/"+testJiraCloudID+"/**", p.Endpoints[0].Path)
	assert.Equal(t, "read-only", p.Endpoints[0].Access)
	assert.NotContains(t, string(data), "atlassian.com\"")
	assert.NotContains(t, string(data), "atlassian.net\"")
	assert.Contains(t, string(data), "tracker.jira.auth: oidc")

	_, _, err = renderJiraSiteProfileFor("BAD", &gw)
	require.Error(t, err)
}

func TestResolveJiraMintCloudID_OIDC(t *testing.T) {
	tracking := oidcTracking(t)
	var gotCfg jira.ScopedConfig
	discover := func(_ context.Context, cfg jira.ScopedConfig) (string, error) {
		gotCfg = cfg
		return oidcJiraGateway + "/ex/jira/" + testJiraCloudID, nil
	}
	id, err := resolveJiraMintCloudID(context.Background(), "", tracking, false, discover)
	require.NoError(t, err)
	assert.Equal(t, testJiraCloudID, id)
	assert.Equal(t, oidcJiraGateway, gotCfg.APIURL, "the lookup is for the configured gateway")

	// A Jira-sourced run reuses its JIRA_API_URL, which points at the gateway.
	id, err = resolveJiraMintCloudID(context.Background(), oidcJiraGateway+"/ex/jira/"+testJiraCloudID, tracking, false, nil)
	require.NoError(t, err)
	assert.Equal(t, testJiraCloudID, id)

	_, err = resolveJiraMintCloudID(context.Background(), jira.DefaultAPIURL+"/ex/jira/"+testJiraCloudID, tracking, false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not under the configured gateway")

	bad := *tracking.Jira
	bad.APIURL = "https://api.atlassian.com"
	_, err = resolveJiraMintCloudID(context.Background(), "", &config.TrackingConfig{Jira: &bad}, false, discover)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Atlassian host")
}

// stubJiraOIDCProvider is stubJiraProvider for the oidc mode: the fetch
// returns JWTs for aud with the given lifetime, and the provider must be
// created with the gateway profile.
func stubJiraOIDCProvider(t *testing.T, aud string, lifetime time.Duration, profileID string) (*jiraProviderCalls, *atomic.Int32) {
	t.Helper()
	calls := stubJiraProvider(t, time.Hour, nil)
	var fetches atomic.Int32
	prevFetch, prevEnsure := jiraOIDCFetchAtFn, ensureJiraProviderLiteralFn
	t.Cleanup(func() { jiraOIDCFetchAtFn, ensureJiraProviderLiteralFn = prevFetch, prevEnsure })
	jiraOIDCFetchAtFn = func(_ context.Context, audience, _, _ string) (*actionsoidc.Assertion, error) {
		n := fetches.Add(1)
		if audience != aud {
			return nil, errors.New("unexpected audience")
		}
		return testOIDCAssertion(aud, lifetime, strings.Repeat("n", int(n))), nil
	}
	ensureJiraProviderLiteralFn = func(_ context.Context, _, providerType string, values map[string]string) error {
		calls.mu.Lock()
		defer calls.mu.Unlock()
		if providerType != profileID {
			return errors.New("unexpected provider type " + providerType)
		}
		calls.creates = append(calls.creates, values)
		return nil
	}
	return calls, &fetches
}

func TestEnsureJiraProviderFrom_OIDCProviderCarriesTheMaskedJWT(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	gw := jiraOIDCGateway{host: "jira-gw.example.com"}
	profileID := jiraGatewaySiteProfileID(testJiraCloudID, gw)
	calls, fetches := stubJiraOIDCProvider(t, oidcJiraAudience, 5*time.Minute, profileID)

	r, w, err := os.Pipe()
	require.NoError(t, err)
	prev := os.Stderr
	os.Stderr = w
	var buf syncBuffer
	h, ensureErr := ensureJiraProviderFrom(context.Background(), harness.ProviderDef{Name: "jira", Type: jiraProviderType}, profileID, "fs-x-0123456789abcdef",
		jiraProviderHandle{audience: oidcJiraAudience, egressHost: gw.host}, ui.New(&buf))
	os.Stderr = prev
	require.NoError(t, w.Close())
	out, _ := io.ReadAll(r)
	require.NoError(t, ensureErr)

	assert.Equal(t, int32(1), fetches.Load())
	assert.Equal(t, int32(0), calls.mints.Load(), "no client_credentials mint")
	require.Len(t, calls.creates, 1)
	tok := calls.creates[0][jiraCredentialKey]
	assert.True(t, testOIDCTokenValid(tok, oidcJiraAudience), "the provider literal is the OIDC JWT")
	assert.Equal(t, "jira-gw.example.com", h.egressHost)
	assert.Equal(t, oidcJiraAudience, h.audience)
	assert.Equal(t, 5*time.Minute, h.expiresAt.Sub(h.issuedAt), "iat and exp come from the JWT")
	assert.Contains(t, string(out), "::add-mask::"+tok)
	assert.NotContains(t, buf.String(), tok)
	assert.NotContains(t, security.NewSecretRedactor().Scan("x "+tok).Sanitized, tok, "the JWT is redacted")
	assert.Contains(t, buf.String(), "GitHub Actions OIDC token")
}

func TestEnsureJiraProviderFrom_OIDCRefusesUnusableTokens(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	calls, _ := stubJiraOIDCProvider(t, oidcJiraAudience, 5*time.Minute, "p")
	src := jiraProviderHandle{audience: oidcJiraAudience, egressHost: "jira-gw.example.com"}
	pd := harness.ProviderDef{Name: "jira", Type: jiraProviderType}

	jiraOIDCFetchAtFn = func(context.Context, string, string, string) (*actionsoidc.Assertion, error) {
		return nil, jira.ErrOIDCEndpointMissing
	}
	_, err := ensureJiraProviderFrom(context.Background(), pd, "p", "fs-x-0123456789abcdef", src, ui.New(io.Discard))
	require.ErrorIs(t, err, jira.ErrOIDCEndpointMissing)
	assert.Contains(t, err.Error(), "id-token: write")

	jiraOIDCFetchAtFn = func(context.Context, string, string, string) (*actionsoidc.Assertion, error) {
		return nil, errors.New("endpoint 500")
	}
	_, err = ensureJiraProviderFrom(context.Background(), pd, "p", "fs-x-0123456789abcdef", src, ui.New(io.Discard))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fetching the OIDC token for the Jira gateway")

	jiraOIDCFetchAtFn = func(context.Context, string, string, string) (*actionsoidc.Assertion, error) {
		return &actionsoidc.Assertion{Value: "not a jwt\n::x::", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}, nil
	}
	_, err = ensureJiraProviderFrom(context.Background(), pd, "p", "fs-x-0123456789abcdef", src, ui.New(io.Discard))
	require.ErrorIs(t, err, errJiraOIDCNotJWT)

	now := time.Now()
	jiraOIDCFetchAtFn = func(context.Context, string, string, string) (*actionsoidc.Assertion, error) {
		return &actionsoidc.Assertion{Value: testOIDCJWT("a", now, now, "z"), IssuedAt: now, ExpiresAt: now}, nil
	}
	_, err = ensureJiraProviderFrom(context.Background(), pd, "p", "fs-x-0123456789abcdef", src, ui.New(io.Discard))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no exp after its iat")
	assert.Empty(t, calls.creates)
}

func TestJiraRefreshDelay_OIDCUsesTokenLifetime(t *testing.T) {
	now := time.Now()
	var buf syncBuffer
	oidc := jiraProviderHandle{name: "jira-x", audience: "aud"}
	// A one-hour JWT: the lead is half its lifetime, so the refresh is due
	// after about half an hour, minus at most the jitter.
	d := jiraRefreshDelay(oidc, now, now.Add(time.Hour), now, ui.New(&buf))
	assert.LessOrEqual(t, d, 30*time.Minute)
	assert.GreaterOrEqual(t, d, 30*time.Minute-gatewayRefreshJitter)
	assert.Empty(t, buf.String())

	// A token too short for the refresh budget refreshes at once, with a
	// warning that names no token.
	d = jiraRefreshDelay(oidc, now, now.Add(time.Minute), now, ui.New(&buf))
	assert.Equal(t, gatewayRefreshMinDelay, d)
	assert.Contains(t, buf.String(), "too soon for a full refresh budget")

	// A minted access token keeps the OpenAI schedule.
	minted := jiraProviderHandle{name: "jira-x", creds: testJiraCreds()}
	d = jiraRefreshDelay(minted, now, now.Add(time.Hour), now, ui.New(io.Discard))
	assert.LessOrEqual(t, d, time.Hour-openAIRefreshMargin)
}

func TestRunJiraRefresh_OIDCRefetchesFromExp(t *testing.T) {
	shrinkOpenAIRefreshSchedule(t)
	shrinkGatewayRefreshTimers(t)
	calls, fetches := stubJiraOIDCProvider(t, oidcJiraAudience, 2*time.Second, "p")
	now := time.Now()
	h := jiraProviderHandle{name: "jira-abc", sandbox: "fs-x", audience: oidcJiraAudience, egressHost: "jira-gw.example.com",
		issuedAt: now, expiresAt: now.Add(2 * time.Second),
		sandboxUp: &atomic.Bool{}, state: &jiraSeedState{}, unrecorded: &seedGenerations{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runJiraRefresh(ctx, h, ui.New(io.Discard))
	}()
	require.Eventually(t, func() bool { return len(calls.updateList()) >= 2 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	<-done
	assert.GreaterOrEqual(t, fetches.Load(), int32(2))
	assert.Equal(t, int32(0), calls.mints.Load())
	for _, u := range calls.updateList() {
		assert.True(t, testOIDCTokenValid(u.values[jiraCredentialKey], oidcJiraAudience))
		assert.WithinDuration(t, time.Now().Add(2*time.Second), u.expiresAt, 3*time.Second, "the provider expires with the JWT")
	}
}

func TestRunJiraRefresh_OIDCMissingEndpointFailsClosed(t *testing.T) {
	shrinkOpenAIRefreshSchedule(t)
	shrinkGatewayRefreshTimers(t)
	calls, _ := stubJiraOIDCProvider(t, oidcJiraAudience, time.Second, "p")
	jiraOIDCFetchAtFn = func(context.Context, string, string, string) (*actionsoidc.Assertion, error) {
		return nil, jira.ErrOIDCEndpointMissing
	}
	now := time.Now()
	h := jiraProviderHandle{name: "jira-abc", sandbox: "fs-x", audience: oidcJiraAudience, issuedAt: now, expiresAt: now.Add(time.Second),
		sandboxUp: &atomic.Bool{}, state: &jiraSeedState{}, unrecorded: &seedGenerations{}}
	var buf syncBuffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		runJiraRefresh(context.Background(), h, ui.New(&buf))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresher did not stop")
	}
	assert.Contains(t, buf.String(), "id-token: write")
	assert.Len(t, calls.expiryList(), 1, "the provider's expiry moves back to the held token's")
}

func TestJiraOIDCTokenHook_MasksInProcessTokens(t *testing.T) {
	clearJiraSecrets(t)
	t.Setenv("GITHUB_ACTIONS", "true")
	g := newFakeJiraGateway(t)
	var calls atomic.Int32
	fetch := jira.WithOIDCFetch(oidcFetchCounting(oidcJiraAudience, 5*time.Minute, &calls))
	setJiraScopedTestOptions(t, jira.WithHTTPClient(g.client()), fetch)

	r, w, err := os.Pipe()
	require.NoError(t, err)
	prev := os.Stderr
	os.Stderr = w
	// `fullsend issues`, run status, the run label and the finalizer share
	// newJiraTrackerClient; the poller uses buildJiraClient.
	tc, err := newJiraTrackerClientFromConfig(oidcTracking(t))
	if err == nil {
		_, err = tc.(*tracker.JiraClient).AuthenticatedUser(context.Background())
	}
	var pollErr error
	j := oidcTracking(t).Jira
	pc, pollErr := buildJiraClient(jiraPollSettings{baseURL: j.BaseURL, apiURL: j.APIURL, auth: j.Auth, audience: j.Audience}, os.Getenv,
		jira.WithHTTPClient(g.client()), fetch)
	if pollErr == nil {
		_, pollErr = pc.GetMyself(context.Background())
	}
	os.Stderr = prev
	require.NoError(t, w.Close())
	out, _ := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, pollErr)
	seen := g.seen()
	require.Len(t, seen, 2)
	for _, tok := range seen {
		assert.Contains(t, string(out), "::add-mask::"+tok)
		assert.NotContains(t, security.NewSecretRedactor().Scan("x "+tok).Sanitized, tok)
	}
}

func TestCheckJiraOIDCRunStart_LeftoverSecretsAreNamedAndRefused(t *testing.T) {
	require.NoError(t, checkJiraOIDCRunStart(nil, jiraEnv(nil)))
	require.NoError(t, checkJiraOIDCRunStart(parseTestConfig(t, testJiraConfig).IssueTracking(), jiraEnv(map[string]string{jira.TokenEnvVar: "t"})),
		"other modes are untouched")
	err := checkJiraOIDCRunStart(oidcTracking(t), jiraEnv(map[string]string{jira.TokenEnvVar: "t0ken", jira.EmailEnvVar: "a@b"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FULLSEND_JIRA_TOKEN, FULLSEND_JIRA_EMAIL are set")
	assert.Contains(t, err.Error(), "removed from the repository and organization secrets")
	assert.NotContains(t, err.Error(), "t0ken")
	require.NoError(t, checkJiraOIDCRunStart(oidcTracking(t), jiraEnv(nil)))
}

func TestJiraProviderOIDC_UsesTheEndpointCapturedAtCreation(t *testing.T) {
	stubJiraOIDCProvider(t, oidcJiraAudience, 5*time.Minute, "p")
	var gotURL, gotToken string
	jiraOIDCFetchAtFn = func(_ context.Context, audience, reqURL, reqToken string) (*actionsoidc.Assertion, error) {
		gotURL, gotToken = reqURL, reqToken
		return testOIDCAssertion(audience, 5*time.Minute, "c"), nil
	}
	t.Setenv(jira.OIDCRequestURLEnvVar, "https://later.example.com")
	h := jiraProviderHandle{audience: oidcJiraAudience, oidcRequestURL: "https://captured.actions.githubusercontent.com", oidcRequestToken: "captured"}
	_, err := h.mintToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "https://captured.actions.githubusercontent.com", gotURL)
	assert.Equal(t, "captured", gotToken)

	// An already-expired assertion is refused.
	jiraOIDCFetchAtFn = func(context.Context, string, string, string) (*actionsoidc.Assertion, error) {
		iat := time.Now().Add(-time.Hour)
		return &actionsoidc.Assertion{Value: testOIDCJWT("a", iat, iat.Add(time.Minute), "e"), IssuedAt: iat, ExpiresAt: iat.Add(time.Minute)}, nil
	}
	_, err = h.mintToken(context.Background())
	require.ErrorContains(t, err, "already expired")
}

func TestRunJiraRefresh_OIDCNotJWTFailsClosed(t *testing.T) {
	shrinkOpenAIRefreshSchedule(t)
	shrinkGatewayRefreshTimers(t)
	calls, _ := stubJiraOIDCProvider(t, oidcJiraAudience, time.Second, "p")
	jiraOIDCFetchAtFn = func(context.Context, string, string, string) (*actionsoidc.Assertion, error) {
		return &actionsoidc.Assertion{Value: "not-a-jwt", IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}, nil
	}
	now := time.Now()
	h := jiraProviderHandle{name: "jira-abc", sandbox: "fs-x", audience: oidcJiraAudience, issuedAt: now, expiresAt: now.Add(time.Second),
		sandboxUp: &atomic.Bool{}, state: &jiraSeedState{}, unrecorded: &seedGenerations{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runJiraRefresh(context.Background(), h, ui.New(io.Discard))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresher did not stop on a non-JWT token")
	}
	assert.Len(t, calls.expiryList(), 1)
}

// TestCheckJiraOIDCRunStart_InvalidAuthFailsEveryRun: fullsend run loads
// config without Validate, so the run-start check validates auth and
// audience on every run, whatever its source: a wrong-case or unknown auth
// value, or an audience without auth: oidc, fails it before any stage (and
// so before any request), even with a static secret that would otherwise
// be used.
func TestCheckJiraOIDCRunStart_InvalidAuthFailsEveryRun(t *testing.T) {
	static := jiraEnv(map[string]string{jira.TokenEnvVar: "static-token"})
	for name, tc := range map[string]struct{ extra, want string }{
		"wrong-case auth value is refused":       {"\n    auth: OIDC\n    audience: aud", `auth "OIDC" is not supported`},
		"unknown auth value is refused":          {"\n    auth: jwt\n    audience: aud", `auth "jwt" is not supported`},
		"audience without auth: oidc is refused": {"\n    audience: aud", "audience is set but auth is not"},
	} {
		t.Run(name, func(t *testing.T) {
			tracking := parseTestConfig(t, "tracker:\n  jira:\n    base_url: https://site.atlassian.net\n    api_url: https://jira-gw.example.com\n    project: ABC"+tc.extra+"\n").IssueTracking()
			err := checkJiraOIDCRunStart(tracking, static)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.NotContains(t, err.Error(), "static-token")
		})
	}
	require.NoError(t, checkJiraOIDCRunStart(parseTestConfig(t, testJiraConfig).IssueTracking(), static), "no auth or audience: unchanged")
}

func TestParseJiraOIDCGateway_RefusesDotSegmentsAndEscapedPaths(t *testing.T) {
	for _, raw := range []string{
		"https://jira-gw.example.com/a/../b",
		"https://jira-gw.example.com/./a",
		"https://jira-gw.example.com/..",
		"https://jira-gw.example.com/a%2Fb",
	} {
		_, err := parseJiraOIDCGateway(raw)
		require.Error(t, err, raw)
	}
}
