package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const jiraOIDCYAML = `version: "1"
tracker:
  jira:
    base_url: https://site.atlassian.net
    api_url: https://jira-gw.example.com
    auth: oidc
    audience: jira-gateway.example
    project: ABC
`

func TestJiraTracking_OIDCKeysParseAndSurviveCloneAndMerge(t *testing.T) {
	cfg, err := ParsePerRepoConfigWriter([]byte(jiraOIDCYAML))
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	j := cfg.IssueTracking().Jira
	require.NotNil(t, j)
	assert.True(t, j.IsOIDC())
	assert.Equal(t, "jira-gateway.example", j.Audience)
	assert.True(t, j.HasConnectionSettings())

	// Clone and the managed (repos.yaml config:) form keep both keys.
	assert.Equal(t, j, cloneTracking(cfg.IssueTracking()).Jira)
	var m ManagedConfig
	require.NoError(t, yaml.Unmarshal([]byte(`tracker:
  jira:
    base_url: https://site.atlassian.net
    api_url: https://jira-gw.example.com
    auth: oidc
    audience: aud
    project: ABC
`), &m))
	got := MergeManaged(&perRepoConfig{}, m.Writer()).IssueTracking().Jira
	assert.Equal(t, JiraAuthOIDC, got.Auth)
	assert.Equal(t, "aud", got.Audience)
}

func TestJiraTracking_OIDCConfigThatCouldReachAtlassianIsRefused(t *testing.T) {
	const site = "\n    base_url: https://site.atlassian.net\n    project: ABC"
	cases := []struct {
		name    string
		jira    string
		wantErr string
	}{
		{name: "unset auth stays valid", jira: site + "\n    api_url: https://jira-gw.example.com"},
		{name: "oidc without audience", jira: site + "\n    api_url: https://jira-gw.example.com\n    auth: oidc", wantErr: "requires audience"},
		{name: "audience without oidc", jira: site + "\n    audience: aud", wantErr: "audience is set but auth is not \"oidc\""},
		{name: "unknown auth", jira: site + "\n    auth: OIDC\n    audience: aud", wantErr: "auth \"OIDC\" is not supported"},
		{name: "audience with space", jira: site + "\n    api_url: https://jira-gw.example.com\n    auth: oidc\n    audience: a b", wantErr: "audience must not contain whitespace"},
		{name: "oidc without api_url", jira: site + "\n    auth: oidc\n    audience: aud", wantErr: "requires api_url"},
		{name: "oidc with http api_url", jira: site + "\n    api_url: http://jira-gw.example.com\n    auth: oidc\n    audience: aud", wantErr: "api_url must be an https URL"},
		{name: "oidc with loopback http api_url", jira: site + "\n    api_url: http://127.0.0.1:8080\n    auth: oidc\n    audience: aud", wantErr: "api_url must be an https URL"},
		{name: "api.atlassian.com as the gateway is refused", jira: site + "\n    api_url: https://api.atlassian.com\n    auth: oidc\n    audience: aud", wantErr: "not an Atlassian host"},
		{name: "oidc to atlassian.net upper with dot", jira: site + "\n    api_url: https://Other.ATLASSIAN.net./\n    auth: oidc\n    audience: aud", wantErr: "ASCII DNS name"},
		{name: "oidc to atlassian.net upper", jira: site + "\n    api_url: https://Other.ATLASSIAN.net/\n    auth: oidc\n    audience: aud", wantErr: "not an Atlassian host"},
		{name: "oidc to atlassian.com subdomain", jira: site + "\n    api_url: https://gw.atlassian.com\n    auth: oidc\n    audience: aud", wantErr: "not an Atlassian host"},
		{name: "base_url host as the gateway is refused", jira: "\n    base_url: https://jira.example.com\n    project: ABC\n    api_url: https://JIRA.example.com/gw\n    auth: oidc\n    audience: aud", wantErr: "must not be base_url's host"},
		{name: "Unicode-dot (U+3002) Atlassian host is refused", jira: site + "\n    api_url: https://api.atlassian\u3002com\n    auth: oidc\n    audience: aud", wantErr: "ASCII DNS name"},
		{name: "Unicode-dot (U+FF0E) Atlassian host is refused", jira: site + "\n    api_url: https://api.atlassian\uff0ecom\n    auth: oidc\n    audience: aud", wantErr: "ASCII DNS name"},
		{name: "Unicode-dot (U+FF61) Atlassian host is refused", jira: site + "\n    api_url: https://api.atlassian\uff61com\n    auth: oidc\n    audience: aud", wantErr: "ASCII DNS name"},
		{name: "trailing dot", jira: site + "\n    api_url: https://jira-gw.example.com.\n    auth: oidc\n    audience: aud", wantErr: "ASCII DNS name"},
		{name: "base_url host written with Unicode dots is still base_url's host", jira: "\n    base_url: https://jira\u3002example.com\n    project: ABC\n    api_url: https://jira.example.com\n    auth: oidc\n    audience: aud", wantErr: "must not be base_url's host"},
		{name: "auth alone needs connection", jira: "\n    auth: oidc\n    audience: aud", wantErr: "base_url is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := ParsePerRepoConfigWriter([]byte("version: \"1\"\ntracker:\n  jira: " + c.jira + "\n"))
			require.NoError(t, err)
			err = cfg.Validate()
			if c.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErr)
		})
	}
}
