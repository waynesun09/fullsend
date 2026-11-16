package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/forge/jira"
)

// TestJiraPollOIDCHint_OldCallerNamesTheReRender: a poll job without an
// OIDC endpoint in oidc mode (an older caller that does not grant
// id-token: write) fails with the id-token: write error and how to fix it:
// re-render the caller with `fullsend repos install`.
func TestJiraPollOIDCHint_OldCallerNamesTheReRender(t *testing.T) {
	settings := jiraPollSettings{baseURL: "https://site.atlassian.net", apiURL: "https://jira-gw.example.com", auth: jira.AuthOIDC, audience: "aud"}
	_, err := buildJiraClient(settings, func(string) string { return "" })
	require.ErrorIs(t, err, jira.ErrOIDCEndpointMissing)
	err = jiraPollOIDCHint(err)
	require.ErrorIs(t, err, jira.ErrOIDCEndpointMissing)
	assert.Contains(t, err.Error(), "id-token: write")
	assert.Contains(t, err.Error(), "re-render the managed poll caller with `fullsend repos install`")
	assert.NotContains(t, err.Error(), "host script", "the poll job's message replaces the host-script advice")

	other := fmt.Errorf("x: %w", errors.New("other"))
	assert.Same(t, other, jiraPollOIDCHint(other), "other errors are unchanged")
}
