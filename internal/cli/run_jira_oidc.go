package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/fullsend-ai/fullsend/internal/config"
	"github.com/fullsend-ai/fullsend/internal/forge/jira"
	"github.com/fullsend-ai/fullsend/internal/harness"
	"github.com/fullsend-ai/fullsend/internal/inference/actionsoidc"
	"github.com/fullsend-ai/fullsend/internal/randutil"
	"github.com/fullsend-ai/fullsend/internal/sandbox"
	"github.com/fullsend-ai/fullsend/internal/security"
	"github.com/fullsend-ai/fullsend/internal/ui"
)

// Jira in the sandbox with tracker.jira.auth: oidc. The run-scoped
// fullsend-jira provider carries the job's GitHub Actions OIDC token for
// tracker.jira.audience instead of a Jira secret, and its per-site profile
// binds the self-hosted gateway (tracker.jira.api_url) with path
// /ex/jira/<cloudId>/**. The token is refreshed from its own iat and exp,
// on the inference gateway route's schedule (gatewayRefreshDelay), and the
// sandbox sees only its placeholder, as with a minted access token.

// jiraOIDCFetchAtFn fetches the job's OIDC token for audience from the
// runner's token endpoint reqURL with reqToken. Callers capture the
// endpoint once, so a later stage that changes the process environment
// cannot change it. Override in tests.
var jiraOIDCFetchAtFn = func(ctx context.Context, audience, reqURL, reqToken string) (*actionsoidc.Assertion, error) {
	if reqURL == "" || reqToken == "" {
		return nil, jira.ErrOIDCEndpointMissing
	}
	return actionsoidc.FetchAssertion(ctx, actionsoidc.AssertionConfig{
		Audience:         audience,
		OIDCRequestURL:   reqURL,
		OIDCRequestToken: reqToken,
	})
}

// jiraOIDCEndpoint returns the runner's OIDC token endpoint from the
// process environment.
func jiraOIDCEndpoint() (reqURL, reqToken string) {
	return os.Getenv(jira.OIDCRequestURLEnvVar), os.Getenv(jira.OIDCRequestTokenEnvVar)
}

// jiraOIDCFetchFn fetches the job's OIDC token for audience with the
// endpoint in the process environment now. Override in tests.
var jiraOIDCFetchFn = func(ctx context.Context, audience string) (*actionsoidc.Assertion, error) {
	reqURL, reqToken := jiraOIDCEndpoint()
	return jiraOIDCFetchAtFn(ctx, audience, reqURL, reqToken)
}

// jiraOIDCTokenHook registers an OIDC token a Jira client fetched in
// process for redaction and, in GitHub Actions, the log mask
// (jira.WithOIDCTokenHook).
func jiraOIDCTokenHook(token string) {
	security.RegisterRuntimeSecret(token)
	maskActionsValue(token)
}

// checkJiraOIDCRunStart runs at the start of every fullsend run, before
// any stage, whatever the run's source. When tracker.jira sets auth or
// audience it validates the connection settings (fullsend run loads config
// without Validate), so a wrong-case or unknown auth value, or an audience
// without auth: oidc, fails the run instead of being ignored. In oidc mode
// it then fails on GitLab CI, or when any FULLSEND_JIRA_* secret is still
// set (the message names them).
func checkJiraOIDCRunStart(tracking *config.TrackingConfig, getenv func(string) string) error {
	if tracking == nil || tracking.Jira == nil {
		return nil
	}
	if j := tracking.Jira; j.Auth != "" || j.Audience != "" {
		if err := j.ValidateConnection(); err != nil {
			return err
		}
	}
	if !tracking.Jira.IsOIDC() {
		return nil
	}
	return jiraOIDCRunPreflight(getenv)
}

// jiraProviderToken is a token for the run-scoped Jira provider with its
// iat (the mint time for an access token) and expiry.
type jiraProviderToken struct {
	Value     string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// errJiraOIDCNotJWT is a refresh failure no retry can fix.
var errJiraOIDCNotJWT = errors.New("the OIDC token for the Jira gateway is not a compact JWT; refusing to use it")

// mintToken returns a new token for the provider: the job's OIDC token for
// h.audience when set, otherwise a minted access token for h.creds.
// Errors never carry a token.
func (h jiraProviderHandle) mintToken(ctx context.Context) (jiraProviderToken, error) {
	if h.audience == "" {
		issued := time.Now()
		tok, err := jiraMintFn(ctx, h.creds)
		if err != nil {
			return jiraProviderToken{}, err
		}
		return jiraProviderToken{Value: tok.Value, IssuedAt: issued, ExpiresAt: tok.ExpiresAt}, nil
	}
	a, err := jiraOIDCFetchAtFn(ctx, h.audience, h.oidcRequestURL, h.oidcRequestToken)
	if err != nil {
		if errors.Is(err, jira.ErrOIDCEndpointMissing) {
			return jiraProviderToken{}, err
		}
		return jiraProviderToken{}, fmt.Errorf("fetching the OIDC token for the Jira gateway (audience %q): %w", h.audience, err)
	}
	if a == nil || !gatewayCompactJWTPattern.MatchString(a.Value) {
		return jiraProviderToken{}, errJiraOIDCNotJWT
	}
	if !a.ExpiresAt.After(a.IssuedAt) {
		return jiraProviderToken{}, errors.New("the OIDC token for the Jira gateway has no exp after its iat")
	}
	if !time.Now().Before(a.ExpiresAt) {
		return jiraProviderToken{}, errors.New("the OIDC token for the Jira gateway has already expired")
	}
	return jiraProviderToken{Value: a.Value, IssuedAt: a.IssuedAt, ExpiresAt: a.ExpiresAt}, nil
}

// jiraRefreshDelay is how long the refresher waits before replacing the
// held token. An OIDC token (h.audience set) follows gatewayRefreshDelay,
// derived from its own iat and exp; a minted access token keeps
// openAIRefreshDelay.
func jiraRefreshDelay(h jiraProviderHandle, issuedAt, expiresAt, now time.Time, printer *ui.Printer) time.Duration {
	if h.audience != "" {
		delay, fits := gatewayRefreshDelay(issuedAt, expiresAt, now, randutil.Float64())
		if !fits {
			printer.StepWarn(fmt.Sprintf("Jira gateway token for %s expires %s, too soon for a full refresh budget; refreshing as early as possible", h.name, expiresAt.UTC().Format(time.RFC3339)))
		}
		return delay
	}
	jitter := time.Duration(0)
	if openAIRefreshJitter > 0 {
		jitter = time.Duration(randutil.Int64N(int64(openAIRefreshJitter)))
	}
	return openAIRefreshDelay(expiresAt, now, jitter)
}

// jiraProviderMints decides how the fullsend-jira provider pd gets its
// value. Without oidc it is jiraMintCredentials. With tracker.jira.auth:
// oidc a definition that declares JIRA_TOKEN carries the job's OIDC token
// (mint=true), and one whose JIRA_TOKEN expands to a value is refused: a
// static token next to the gateway route would be a second credential.
func jiraProviderMints(pd harness.ProviderDef, oidc bool, getenv func(string) string) (jira.Credentials, bool, error) {
	if !oidc {
		return jiraMintCredentials(pd, getenv)
	}
	v, declared := pd.Credentials[jiraCredentialKey]
	if !declared {
		return jira.Credentials{}, false, nil
	}
	if !sandbox.ProviderCredentialEmpty(v) {
		return jira.Credentials{}, false, fmt.Errorf("provider %q: tracker.jira.auth is %q, so its %s must not expand to a static token; the run gives it the job's OIDC token for the Jira gateway",
			pd.Name, config.JiraAuthOIDC, jiraCredentialKey)
	}
	return jira.Credentials{}, true, nil
}

// jiraOIDCRunPreflight applies the oidc mode's fail-closed rules before
// the provider is created: GitLab CI is not supported yet, and no
// FULLSEND_JIRA_* secret may be set. A missing OIDC endpoint fails the
// first fetch (jira.ErrOIDCEndpointMissing).
func jiraOIDCRunPreflight(getenv func(string) string) error {
	if getenv("GITLAB_CI") == "true" {
		return fmt.Errorf("tracker.jira.auth: %s is not supported on GitLab CI yet", config.JiraAuthOIDC)
	}
	return jira.CheckNoCredentialEnv(getenv)
}

// jiraOIDCGateway is the self-hosted gateway a per-site profile binds with
// tracker.jira.auth: oidc: a bare host and the URL's path prefix ("" for a
// bare origin).
type jiraOIDCGateway struct {
	host   string
	prefix string
}

// jiraGatewayPathPrefixRe is a gateway path prefix safe to render into a
// profile path: slash-separated plain segments, no glob or YAML character.
var jiraGatewayPathPrefixRe = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)*$`)

// parseJiraOIDCGateway reads the gateway the sandbox profile binds from
// apiURL (tracker.jira.api_url). It must be https on port 443 with a bare
// DNS host (validateGatewayHost) and a plain path prefix. Errors never
// include the raw URL.
func parseJiraOIDCGateway(apiURL string) (jiraOIDCGateway, error) {
	u, err := url.Parse(strings.TrimRight(apiURL, "/"))
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return jiraOIDCGateway{}, errors.New("tracker.jira.api_url must be an https URL without credentials, query, or fragment")
	}
	if p := u.Port(); p != "" && p != "443" {
		return jiraOIDCGateway{}, fmt.Errorf("tracker.jira.api_url uses port %s; the sandbox's Jira profile reaches the gateway on port 443 only", p)
	}
	host, err := validateGatewayHost(u.Hostname())
	if err != nil {
		return jiraOIDCGateway{}, fmt.Errorf("tracker.jira.api_url: %w", err)
	}
	if u.RawPath != "" || !jiraGatewayPathPrefixRe.MatchString(u.Path) {
		return jiraOIDCGateway{}, errors.New("tracker.jira.api_url has a path with characters the sandbox's Jira profile cannot bind")
	}
	// A "." or ".." segment would make the profile path and the path the
	// client requests differ.
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return jiraOIDCGateway{}, errors.New("tracker.jira.api_url has a \".\" or \"..\" path segment")
		}
	}
	return jiraOIDCGateway{host: host, prefix: u.Path}, nil
}

// jiraGatewaySiteProfileID is jiraSiteProfileID for a site reached through
// the self-hosted gateway gw: the hash also covers gw, so a site's profile
// for one gateway never replaces its profile for another (or for
// api.atlassian.com) on a shared OpenShell gateway. It has the same shape,
// so jiraSiteProfileIDPattern reserves it too.
func jiraGatewaySiteProfileID(cloudID string, gw jiraOIDCGateway) string {
	sum := sha256.Sum256([]byte(cloudID + "\x00" + gw.host + gw.prefix))
	return jiraSiteProfileIDPrefix + hex.EncodeToString(sum[:])[:gatewayProfileIDHashLen]
}
