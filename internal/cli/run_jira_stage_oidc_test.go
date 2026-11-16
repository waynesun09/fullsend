package cli

import (
	"context"
	"io"
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

	"github.com/fullsend-ai/fullsend/internal/forge/jira"
	"github.com/fullsend-ai/fullsend/internal/ui"
)

// TestJiraStage_OIDCModePostsThroughTheGatewayWithTheOIDCToken: in oidc
// mode the stage comment goes to the self-hosted gateway at
// /ex/jira/<cloudId>/rest/api/3/... with a Bearer JWT for the configured
// audience, and no request reaches an Atlassian host.
func TestJiraStage_OIDCModePostsThroughTheGatewayWithTheOIDCToken(t *testing.T) {
	clearJiraSecrets(t)
	t.Setenv("GITHUB_ACTIONS", "")

	type seenRequest struct{ host, method, path, auth string }
	var (
		mu       sync.Mutex
		requests []seenRequest
	)
	api := "/ex/jira/" + testJiraCloudID + "/rest/api/3"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, seenRequest{r.Host, r.Method, r.URL.Path, r.Header.Get("Authorization")})
		mu.Unlock()
		if r.URL.Path == "/_edge/tenant_info" && r.Host == "site.example.com" {
			_, _ = w.Write([]byte(`{"cloudId":"` + testJiraCloudID + `"}`))
			return
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.Host != "jira-gw.example.com" || !ok || !testOIDCTokenValid(tok, oidcJiraAudience) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == api+"/myself":
			_, _ = w.Write([]byte(`{"accountId":"fake-bot-account-id","active":true}`))
		case r.Method == http.MethodGet && r.URL.Path == api+"/issue/PROJ-42/comment":
			_, _ = w.Write([]byte(`{"comments":[],"total":0,"startAt":0,"maxResults":100}`))
		case r.Method == http.MethodPost && r.URL.Path == api+"/issue/PROJ-42/comment":
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"100"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	tr := srv.Client().Transport.(*http.Transport).Clone()
	addr := srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	var fetches atomic.Int32
	setJiraScopedTestOptions(t, jira.WithHTTPClient(&http.Client{Transport: tr, Timeout: 10 * time.Second}),
		jira.WithOIDCFetch(oidcFetchCounting(oidcJiraAudience, 5*time.Minute, &fetches)))

	tracking := parseTestConfig(t, strings.Replace(oidcJiraConfig, "project: ABC", "project: PROJ", 1)).IssueTracking()
	st, cleanup, err := armJiraStage(stageHarness(), stageOpts(), tracking, "github", keepHistoryTrue, jiraEnv(githubStageEnv()), nil)
	require.NoError(t, err)
	require.NotNil(t, st, "oidc mode builds the stage client")
	defer cleanup()
	path := writeStageResult(t, st, `{"verb":"opened","url":"https://github.com/acme/widgets/pull/7"}`)
	require.NoError(t, st.postJiraStageComment(context.Background(), path, ui.New(io.Discard)))

	mu.Lock()
	defer mu.Unlock()
	var posted bool
	for _, r := range requests {
		assert.NotContains(t, r.host, "atlassian", "no request reaches an Atlassian host")
		if r.path == "/_edge/tenant_info" {
			assert.Empty(t, r.auth, "cloud ID discovery carries no credential")
			continue
		}
		assert.Equal(t, "jira-gw.example.com", r.host)
		assert.True(t, strings.HasPrefix(r.path, api+"/"), r.path)
		tok, ok := strings.CutPrefix(r.auth, "Bearer ")
		assert.True(t, ok && testOIDCTokenValid(tok, oidcJiraAudience), "Bearer JWT for the configured audience")
		if r.method == http.MethodPost && r.path == api+"/issue/PROJ-42/comment" {
			posted = true
		}
	}
	assert.True(t, posted, "the stage comment was created through the gateway")
	assert.Positive(t, fetches.Load())
}
