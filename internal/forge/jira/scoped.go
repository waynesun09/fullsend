package jira

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Environment variables carrying the credentials for Fullsend-managed Jira
// API calls. They are repository secrets and never part of config files.
// A deployment sets either FULLSEND_JIRA_TOKEN (scoped API token, with
// FULLSEND_JIRA_EMAIL for a bot user) or FULLSEND_JIRA_CLIENT_ID and
// FULLSEND_JIRA_CLIENT_SECRET (a service account's OAuth 2.0 credential),
// never both.
const (
	// TokenEnvVar holds the scoped Jira API token. Required unless the
	// OAuth 2.0 client credentials are set.
	TokenEnvVar = "FULLSEND_JIRA_TOKEN"
	// EmailEnvVar holds the Atlassian account email of a bot user. Optional:
	// when set, the token is a scoped user token sent with Basic auth; when
	// unset, the token is a scoped service-account token sent as a Bearer
	// token. The token string itself does not reveal its type. It applies
	// only to FULLSEND_JIRA_TOKEN.
	EmailEnvVar = "FULLSEND_JIRA_EMAIL"
	// ClientIDEnvVar holds the client ID of a service account's OAuth 2.0
	// credential. Set together with ClientSecretEnvVar instead of
	// TokenEnvVar.
	ClientIDEnvVar = "FULLSEND_JIRA_CLIENT_ID"
	// ClientSecretEnvVar holds the client secret of a service account's
	// OAuth 2.0 credential. Set together with ClientIDEnvVar instead of
	// TokenEnvVar.
	ClientSecretEnvVar = "FULLSEND_JIRA_CLIENT_SECRET"
)

// DefaultAPIURL is the Atlassian API gateway used when no API URL is
// configured. It matches config.DefaultJiraAPIURL, the default of
// tracker.jira.api_url.
const DefaultAPIURL = "https://api.atlassian.com"

// Credentials are the secrets used by NewScoped: either a scoped API token
// (Token, optionally with Email) or OAuth 2.0 client credentials (ClientID
// and ClientSecret).
type Credentials struct {
	// Token is a scoped API token (user or service account).
	Token string
	// Email selects bot-user Basic auth when non-empty; otherwise the
	// token is sent as a Bearer token (service account).
	Email string
	// ClientID and ClientSecret are a service account's OAuth 2.0
	// credential, exchanged with the client_credentials grant for
	// short-lived Bearer access tokens.
	ClientID     string
	ClientSecret string
}

// OAuth reports whether the credentials are OAuth 2.0 client credentials
// rather than a scoped API token.
func (c Credentials) OAuth() bool {
	return c.ClientID != "" || c.ClientSecret != ""
}

// validate checks that exactly one credential type is fully set. Errors
// name the variables but never their values.
func (c Credentials) validate() error {
	if !c.OAuth() {
		if c.Token == "" {
			return fmt.Errorf("jira: %s is required (or set %s and %s for a service account's OAuth 2.0 credential)",
				TokenEnvVar, ClientIDEnvVar, ClientSecretEnvVar)
		}
		return nil
	}
	if c.Token != "" {
		return fmt.Errorf("jira: set either %s or %s and %s, not both", TokenEnvVar, ClientIDEnvVar, ClientSecretEnvVar)
	}
	if c.Email != "" {
		return fmt.Errorf("jira: %s applies only to %s; unset it when using %s and %s",
			EmailEnvVar, TokenEnvVar, ClientIDEnvVar, ClientSecretEnvVar)
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		return fmt.Errorf("jira: %s and %s must both be set", ClientIDEnvVar, ClientSecretEnvVar)
	}
	return nil
}

// CredentialsFromEnv reads the Jira credentials through getenv, or
// os.Getenv when getenv is nil: FULLSEND_JIRA_TOKEN with optional
// FULLSEND_JIRA_EMAIL, or FULLSEND_JIRA_CLIENT_ID with
// FULLSEND_JIRA_CLIENT_SECRET. Exactly one credential type must be set.
// Surrounding whitespace, such as a trailing newline in a stored secret, is
// trimmed. Errors name the variables but never their values.
func CredentialsFromEnv(getenv func(string) string) (Credentials, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	creds := Credentials{
		Token:        strings.TrimSpace(getenv(TokenEnvVar)),
		Email:        strings.TrimSpace(getenv(EmailEnvVar)),
		ClientID:     strings.TrimSpace(getenv(ClientIDEnvVar)),
		ClientSecret: strings.TrimSpace(getenv(ClientSecretEnvVar)),
	}
	if err := creds.validate(); err != nil {
		return Credentials{}, err
	}
	return creds, nil
}

// ScopedConfig holds the non-secret connection settings for NewScoped,
// taken from the effective tracker.jira config.
type ScopedConfig struct {
	// BaseURL is the Jira site URL (tracker.jira.base_url). It is used to
	// discover the cloud ID and for issue browse links.
	BaseURL string
	// APIURL is the API gateway URL (tracker.jira.api_url); empty means
	// DefaultAPIURL. With AuthOIDC it is required and must be a
	// self-hosted gateway (CheckOIDCGatewayURL).
	APIURL string
	// Auth is "" (credentials from Credentials) or AuthOIDC
	// (tracker.jira.auth).
	Auth string
	// Audience is the OIDC audience requested with AuthOIDC
	// (tracker.jira.audience).
	Audience string
	// OIDCRequestURL and OIDCRequestToken are the runner's
	// ACTIONS_ID_TOKEN_REQUEST_URL and ACTIONS_ID_TOKEN_REQUEST_TOKEN, used
	// with AuthOIDC unless WithOIDCFetch supplies the token.
	OIDCRequestURL   string
	OIDCRequestToken string
}

// NewScoped is the shared constructor for Fullsend-managed Jira API calls
// with scoped API tokens or OAuth 2.0 client credentials. Requests go
// through the API gateway at {APIURL}/ex/jira/{cloudId}/rest/api/3/...,
// with the cloud ID discovered from BaseURL (see WithAPIGateway). With a
// token, auth is Basic email:token when creds.Email is set (bot user) and
// Bearer token otherwise (service account). With client credentials, auth
// is a Bearer access token obtained from the Atlassian token endpoint
// (DefaultOAuthTokenURL, or WithOAuthTokenURL) and refreshed before it
// expires or when the gateway rejects it. With cfg.Auth AuthOIDC, creds
// must be empty and auth is a Bearer GitHub Actions OIDC token for
// cfg.Audience, sent only to a self-hosted gateway at cfg.APIURL and
// refetched before it expires or once when the gateway rejects it. opts
// are applied after the connection settings, for example WithHTTPClient.
func NewScoped(cfg ScopedConfig, creds Credentials, opts ...Option) (*LiveClient, error) {
	switch cfg.Auth {
	case "":
	case AuthOIDC:
		return newScopedOIDC(cfg, creds, opts)
	default:
		return nil, fmt.Errorf("jira: auth %q is not supported; the only accepted value is %q", cfg.Auth, AuthOIDC)
	}
	if err := creds.validate(); err != nil {
		return nil, err
	}
	apiURL := cfg.APIURL
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	all := []Option{
		WithBaseURL(cfg.BaseURL),
		WithAPIGateway(apiURL),
	}
	if creds.OAuth() {
		all = append(all, withOAuthClientCredentials(creds.ClientID, creds.ClientSecret))
		return New("", append(all, opts...)...)
	}
	all = append(all, WithEmail(creds.Email))
	return New(creds.Token, append(all, opts...)...)
}

// newScopedOIDC is NewScoped for AuthOIDC.
func newScopedOIDC(cfg ScopedConfig, creds Credentials, opts []Option) (*LiveClient, error) {
	if creds != (Credentials{}) {
		return nil, fmt.Errorf("jira: tracker.jira.auth is %q, so no Jira credential (%s, %s, %s, %s) may be used", AuthOIDC,
			TokenEnvVar, EmailEnvVar, ClientIDEnvVar, ClientSecretEnvVar)
	}
	audience := strings.TrimSpace(cfg.Audience)
	if audience == "" {
		return nil, errors.New("jira: tracker.jira.auth: oidc requires tracker.jira.audience")
	}
	if err := CheckOIDCGatewayURL(cfg.APIURL, cfg.BaseURL); err != nil {
		return nil, err
	}
	all := []Option{
		WithBaseURL(cfg.BaseURL),
		WithAPIGateway(cfg.APIURL),
		func(c *LiveClient) {
			c.oidc = &oidcTokenSource{
				audience:     audience,
				requestURL:   cfg.OIDCRequestURL,
				requestToken: cfg.OIDCRequestToken,
				sem:          make(chan struct{}, 1),
				now:          time.Now,
			}
		},
	}
	return New("", append(all, opts...)...)
}

// GatewayBaseURL returns the per-site API gateway base URL
// {APIURL}/ex/jira/{cloudId}, with no trailing slash: the base NewScoped
// clients append /rest/api/3/... to. An empty cfg.APIURL means
// DefaultAPIURL. The cloud ID is discovered from cfg.BaseURL with the same
// lookup the shared client uses (GET {BaseURL}/_edge/tenant_info). That
// endpoint is public, so no credentials are needed or sent. opts are
// applied after the connection settings, for example WithHTTPClient.
func GatewayBaseURL(ctx context.Context, cfg ScopedConfig, opts ...Option) (string, error) {
	apiURL := cfg.APIURL
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	all := append([]Option{WithBaseURL(cfg.BaseURL), WithAPIGateway(apiURL)}, opts...)
	c := newLiveClient("", all)
	if err := c.validateEndpoints(); err != nil {
		return "", err
	}
	return c.gatewaySiteURL(ctx)
}
