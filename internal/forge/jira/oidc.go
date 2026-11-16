package jira

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/fullsend-ai/fullsend/internal/inference/actionsoidc"
	"github.com/fullsend-ai/fullsend/internal/urlutil"
)

// AuthOIDC is the ScopedConfig.Auth value (tracker.jira.auth: oidc) that
// authenticates to a self-hosted Jira gateway with the job's GitHub Actions
// OIDC token instead of a Jira secret. The gateway verifies the token and
// calls Jira with its own credential; the token never reaches Atlassian.
const AuthOIDC = "oidc"

// Runner variables GitHub Actions sets when the job has the id-token: write
// permission. They are the only source of a fresh OIDC token.
const (
	OIDCRequestURLEnvVar   = "ACTIONS_ID_TOKEN_REQUEST_URL"
	OIDCRequestTokenEnvVar = "ACTIONS_ID_TOKEN_REQUEST_TOKEN"
)

// maxOIDCExpirySkew caps how long before its exp a cached OIDC token is
// replaced. The skew is also at most a quarter of the token's own
// lifetime (exp - iat), so a short-lived token is still used for most of
// its life.
const maxOIDCExpirySkew = 60 * time.Second

// OIDCFetchFunc returns a GitHub Actions OIDC token (a JWT) with its iat
// and exp. The default, set up by NewScoped, calls
// actionsoidc.FetchAssertion with ScopedConfig.Audience and the runner's
// token request variables.
type OIDCFetchFunc func(ctx context.Context) (*actionsoidc.Assertion, error)

// WithOIDCFetch replaces the OIDC token fetch of a client built with
// AuthOIDC, for example with a fixed token or a test server. It has no
// effect on other clients.
func WithOIDCFetch(fn OIDCFetchFunc) Option {
	return func(c *LiveClient) {
		c.oidcFetch = fn
	}
}

// WithOIDCTokenHook calls hook with every OIDC token a client built with
// AuthOIDC fetches, before the token is used, so the caller can register
// it for redaction and the Actions log mask. It has no effect on other
// clients.
func WithOIDCTokenHook(hook func(token string)) Option {
	return func(c *LiveClient) {
		c.oidcHook = hook
	}
}

// oidcTokenSource caches the OIDC token a client sends as its Bearer
// credential to the gateway.
type oidcTokenSource struct {
	audience     string
	requestURL   string
	requestToken string
	fetch        OIDCFetchFunc
	// hook, when set, sees every fetched token (WithOIDCTokenHook).
	hook func(token string)

	// sem serializes fetches; value and refreshAt cache the last
	// successful one. A failed fetch is not cached.
	sem       chan struct{}
	value     string
	refreshAt time.Time
	now       func() time.Time
	// perRequest fetches on every call instead of caching (the host
	// script broker, OIDCTokenFDEnvVar, which keeps its own cache).
	perRequest bool
	// refetch, when set, replaces fetch for the fetch that replaces a
	// token the gateway refused.
	refetch OIDCFetchFunc
}

// oidcExpirySkew is how long before exp a token with the given lifetime
// (exp - iat) is replaced: a quarter of the lifetime, at most
// maxOIDCExpirySkew.
func oidcExpirySkew(lifetime time.Duration) time.Duration {
	return min(maxOIDCExpirySkew, lifetime/4)
}

// token returns a valid OIDC token, fetching a new one when none is cached,
// the cached one is within its skew of exp, or the cached one equals stale
// (a token the gateway just refused). A fetch that returns stale again
// (a token that cannot be refetched) is an error. Concurrent callers wait
// for a single fetch; waiting honours ctx. Errors never carry the token.
func (s *oidcTokenSource) token(ctx context.Context, stale string) (string, error) {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-s.sem }()
	if !s.perRequest && s.value != "" && s.value != stale && s.now().Before(s.refreshAt) {
		return s.value, nil
	}
	s.value = ""
	fetch := s.fetch
	if stale != "" && s.refetch != nil {
		fetch = s.refetch
	}
	a, err := fetch(ctx)
	if err != nil {
		return "", fmt.Errorf("jira: fetching the OIDC token for the Jira gateway (audience %q): %w", s.audience, err)
	}
	if a == nil || a.Value == "" {
		return "", errors.New("jira: the OIDC token fetch returned no token")
	}
	for i := 0; i < len(a.Value); i++ {
		if a.Value[i] < 0x21 || a.Value[i] > 0x7e {
			return "", errors.New("jira: the OIDC token has characters outside printable ASCII; refusing to send it")
		}
	}
	if stale != "" && a.Value == stale {
		return "", errors.New("jira: the Jira gateway refused the OIDC token and no fresh token can be fetched")
	}
	if !a.ExpiresAt.After(a.IssuedAt) {
		return "", errors.New("jira: the OIDC token's exp is not after its iat")
	}
	if !s.now().Before(a.ExpiresAt) {
		return "", fmt.Errorf("jira: the OIDC token expired at %s", a.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if s.hook != nil {
		s.hook(a.Value)
	}
	s.value = a.Value
	s.refreshAt = a.ExpiresAt.Add(-oidcExpirySkew(a.Lifetime()))
	return a.Value, nil
}

// defaultOIDCFetch returns the fetch NewScoped uses when WithOIDCFetch is
// not given: actionsoidc.FetchAssertion with the source's audience and the
// runner's token request variables. It fails when those are missing, which
// means the job lacks the id-token: write permission.
func (s *oidcTokenSource) defaultOIDCFetch() (OIDCFetchFunc, error) {
	if s.requestURL == "" || s.requestToken == "" {
		return nil, ErrOIDCEndpointMissing
	}
	cfg := actionsoidc.AssertionConfig{
		Audience:         s.audience,
		OIDCRequestURL:   s.requestURL,
		OIDCRequestToken: s.requestToken,
	}
	return func(ctx context.Context) (*actionsoidc.Assertion, error) {
		return actionsoidc.FetchAssertion(ctx, cfg)
	}, nil
}

// ErrOIDCEndpointMissing is returned for tracker.jira.auth: oidc when the
// runner's OIDC token request variables are not set (and, in a host
// script, no token broker fd either).
var ErrOIDCEndpointMissing = fmt.Errorf("jira: tracker.jira.auth: oidc needs the GitHub Actions OIDC token endpoint (%s and %s are not set); "+
	"give the workflow job the `id-token: write` permission, or, in a host script, opt in to %s in the harness env.runner",
	OIDCRequestURLEnvVar, OIDCRequestTokenEnvVar, OIDCTokenFDEnvVar)

// CheckOIDCGatewayURL refuses an API URL that the job's OIDC token must
// not be sent to: one that is empty or not https, or whose host is an
// Atlassian host (api.atlassian.com, *.atlassian.net, *.atlassian.com) or
// baseURL's host. Errors name the host but never a full URL.
func CheckOIDCGatewayURL(apiURL, baseURL string) error {
	if strings.TrimSpace(apiURL) == "" {
		return errors.New("jira: tracker.jira.auth: oidc requires tracker.jira.api_url, the self-hosted Jira gateway URL")
	}
	u, err := url.Parse(apiURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return errors.New("jira: tracker.jira.auth: oidc requires an https tracker.jira.api_url without credentials")
	}
	if !urlutil.IsLDHHost(u.Hostname()) {
		return errors.New("jira: tracker.jira.auth: oidc requires tracker.jira.api_url's host to be an ASCII DNS name (letters, digits and hyphens)")
	}
	if urlutil.IsAtlassianHost(u.Hostname()) {
		return fmt.Errorf("jira: tracker.jira.auth: oidc must not send the OIDC token to the Atlassian host %s; set tracker.jira.api_url to the self-hosted gateway", u.Hostname())
	}
	if site, err := url.Parse(baseURL); err == nil && urlutil.SameHost(site.Hostname(), u.Hostname()) {
		return fmt.Errorf("jira: tracker.jira.auth: oidc must not send the OIDC token to the Jira site host %s; set tracker.jira.api_url to the self-hosted gateway", u.Hostname())
	}
	return nil
}

// credentialEnvVars are the Jira secrets tracker.jira.auth: oidc replaces.
var credentialEnvVars = []string{TokenEnvVar, EmailEnvVar, ClientIDEnvVar, ClientSecretEnvVar}

// CheckNoCredentialEnv fails when any FULLSEND_JIRA_* credential variable
// is non-empty, naming the variables but never their values. With
// tracker.jira.auth: oidc the job holds no Jira secret, and one that is
// still set would be a second, unintended credential. getenv nil means
// os.Getenv.
func CheckNoCredentialEnv(getenv func(string) string) error {
	if getenv == nil {
		getenv = os.Getenv
	}
	var set []string
	for _, k := range credentialEnvVars {
		if strings.TrimSpace(getenv(k)) != "" {
			set = append(set, k)
		}
	}
	if len(set) == 0 {
		return nil
	}
	verb := "are"
	if len(set) == 1 {
		verb = "is"
	}
	return fmt.Errorf("jira: tracker.jira.auth is %q but %s %s set; oidc mode needs them removed from the repository and organization secrets",
		AuthOIDC, strings.Join(set, ", "), verb)
}

// NewScopedFromEnv builds a NewScoped client with credentials from getenv
// (os.Getenv when nil). Without AuthOIDC they come from CredentialsFromEnv.
// With AuthOIDC no FULLSEND_JIRA_* secret may be set (CheckNoCredentialEnv),
// GitLab CI is refused, and the OIDC token is fetched from the runner's
// ACTIONS_ID_TOKEN_REQUEST_URL and ACTIONS_ID_TOKEN_REQUEST_TOKEN. Without
// those, inside a host script given the runner's token broker
// (OIDCTokenFDEnvVar, used only when it is a number of at least 3), a
// token is requested from the broker for every Jira request, and a fresh
// one for the one retry after a 401.
func NewScopedFromEnv(cfg ScopedConfig, getenv func(string) string, opts ...Option) (*LiveClient, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if cfg.Auth != AuthOIDC {
		creds, err := CredentialsFromEnv(getenv)
		if err != nil {
			return nil, err
		}
		return NewScoped(cfg, creds, opts...)
	}
	if getenv("GITLAB_CI") == "true" {
		return nil, fmt.Errorf("jira: tracker.jira.auth: %s is not supported on GitLab CI yet", AuthOIDC)
	}
	if err := CheckNoCredentialEnv(getenv); err != nil {
		return nil, err
	}
	cfg.OIDCRequestURL = getenv(OIDCRequestURLEnvVar)
	cfg.OIDCRequestToken = getenv(OIDCRequestTokenEnvVar)
	if cfg.OIDCRequestURL == "" || cfg.OIDCRequestToken == "" {
		if fd, ok := parseOIDCTokenFD(getenv(OIDCTokenFDEnvVar)); ok {
			// Prepended, so a caller's own WithOIDCFetch still wins.
			opts = append([]Option{withOIDCTokenFD(fd)}, opts...)
		}
	}
	return NewScoped(cfg, Credentials{}, opts...)
}
