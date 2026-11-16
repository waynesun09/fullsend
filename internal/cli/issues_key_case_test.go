package cli

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/tracker"
	"github.com/fullsend-ai/fullsend/internal/ui"
)

// TestIssues_JiraKeysAreUppercased: a self-hosted gateway refuses a
// lowercase project or issue key, so --project, --parent, --from and --to
// are upper-cased for Jira before use.
func TestIssues_JiraKeysAreUppercased(t *testing.T) {
	ctx := context.Background()
	reader := parseTestConfig(t, testJiraConfig)

	tc, fc, err := tracker.NewFakeJiraClientWithFake("https://example.atlassian.net")
	require.NoError(t, err)
	require.NoError(t, runIssuesCreate(ctx, &issuesCreateConfig{
		trackerName: trackerJira, project: " proj ", issueType: "Sub-task", parent: "proj-7", title: "t", body: "b",
		testClient: tc, testConfigReader: reader, testWriter: io.Discard,
	}))
	require.Len(t, fc.CreatedIssues, 1)
	assert.Equal(t, "PROJ", fc.CreatedIssues[0].ProjectKey)
	assert.Equal(t, "PROJ-7", fc.CreatedIssues[0].ParentKey)

	require.NoError(t, runIssuesLink(ctx, &issuesLinkConfig{
		trackerName: trackerJira, from: "proj-1", to: "other-2", linkType: "Blocks",
		testClient: tc, testPrinter: ui.New(io.Discard), testConfigReader: reader,
	}))
	require.Len(t, fc.Links, 1)
	keys := []string{fc.Links[0].InwardIssue, fc.Links[0].OutwardIssue}
	assert.ElementsMatch(t, []string{"PROJ-1", "OTHER-2"}, keys)

	project, err := resolveIssueProject(trackerJira, "proj", "", reader)
	require.NoError(t, err)
	assert.Equal(t, "PROJ", project)
	project, err = resolveIssueProject(trackerGitHub, "Owner/Repo", "", nil)
	require.NoError(t, err)
	assert.Equal(t, "Owner/Repo", project, "other trackers keep their case")
}
