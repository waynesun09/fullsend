package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/config"
	"github.com/fullsend-ai/fullsend/internal/forge/jira"
	"github.com/fullsend-ai/fullsend/internal/harness"
	"github.com/fullsend-ai/fullsend/internal/tracker"
	"github.com/fullsend-ai/fullsend/internal/ui"
)

// clearStageJiraSecrets unsets the FULLSEND_JIRA_* secrets for the test.
func clearStageJiraSecrets(t *testing.T) {
	t.Helper()
	for _, k := range []string{jira.TokenEnvVar, jira.EmailEnvVar, jira.ClientIDEnvVar, jira.ClientSecretEnvVar} {
		t.Setenv(k, "")
	}
}

func stageHarness() *harness.Harness {
	return &harness.Harness{Env: &harness.EnvConfig{Runner: map[string]string{jiraStageResultEnv: "${" + jiraStageResultEnv + "}"}}}
}

func stageOpts() statusOpts {
	return statusOpts{trackerSource: "jira", trackerProject: "PROJ", statusNum: 42}
}

func keepHistoryTrue() bool { return true }

func githubStageEnv() map[string]string {
	return map[string]string{"GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "acme/widgets"}
}

// armTestJiraStage arms the stage comment with a fake Jira client and
// returns it with the fake.
func armTestJiraStage(t *testing.T, forge string, env map[string]string) (*jiraStage, *tracker.FakeJiraClient) {
	t.Helper()
	tc, fc, err := tracker.NewFakeJiraClientWithFake("https://example.atlassian.net")
	require.NoError(t, err)
	prev := jiraStageNewClientFn
	jiraStageNewClientFn = func(*config.TrackingConfig) (*tracker.JiraClient, error) { return tc, nil }
	t.Cleanup(func() { jiraStageNewClientFn = prev })
	st, cleanup, err := armJiraStage(stageHarness(), stageOpts(), nil, forge, keepHistoryTrue, jiraEnv(env), []string{t.TempDir()})
	require.NoError(t, err)
	require.NotNil(t, st)
	t.Cleanup(cleanup)
	return st, fc
}

// writeStageResult writes content to the stage's result file.
func writeStageResult(t *testing.T, st *jiraStage, content string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(st.path, []byte(content), 0o600))
	return st.path
}

func stageComment(t *testing.T, fc *tracker.FakeJiraClient) (string, string) {
	t.Helper()
	comments := fc.Comments["PROJ-42"]
	require.Len(t, comments, 1)
	var marker string
	for _, raw := range fc.CommentProperties["PROJ-42/"+comments[0].ID] {
		var v struct {
			Marker string `json:"marker"`
		}
		_ = json.Unmarshal(raw, &v)
		marker = v.Marker
	}
	body := strings.ReplaceAll(jira.ADFToMarkdown(comments[0].Body), `\_`, "_") // ADFToMarkdown escapes "_"
	return strings.TrimSpace(body), marker
}

func TestJiraStage_PostsOpenedAndPushed(t *testing.T) {
	for verb, want := range map[string]string{
		"opened": "**Code stage:** opened pull request https://github.com/acme/widgets/pull/7 for PROJ-42.\n\nReview and merge happen on the pull request.",
		"pushed": "**Code stage:** pushed new commits to pull request https://github.com/acme/widgets/pull/7 for PROJ-42.\n\nReview and merge happen on the pull request.",
	} {
		t.Run(verb, func(t *testing.T) {
			st, fc := armTestJiraStage(t, "github", githubStageEnv())
			path := writeStageResult(t, st, `{"verb":"`+verb+`","url":"https://github.com/acme/widgets/pull/7"}`+"\n")
			require.NoError(t, st.postJiraStageComment(context.Background(), path, ui.New(io.Discard)))
			body, marker := stageComment(t, fc)
			assert.Equal(t, want, body)
			assert.Equal(t, jiraStageMarker, marker)
		})
	}
}

// TestJiraStage_ForgeURLsWithPortsAndURLRoots: GitLab with a relative URL
// root and a port, and GitHub Enterprise with a port, each take their
// expected prefix and port from the URL captured at start.
func TestJiraStage_ForgeURLsWithPortsAndURLRoots(t *testing.T) {
	cases := []struct {
		name, forge string
		env         map[string]string
		ok, bad     string
	}{
		{"gitlab with URL root and port", "gitlab",
			map[string]string{"CI_PROJECT_URL": "https://gitlab.example.com:8443/gitlab/grp/sub/proj"},
			"https://gitlab.example.com:8443/gitlab/grp/sub/proj/-/merge_requests/3",
			"https://gitlab.example.com/gitlab/grp/sub/proj/-/merge_requests/3"},
		{"gitlab default port", "gitlab",
			map[string]string{"CI_PROJECT_URL": "https://gitlab.example.com/grp/proj"},
			"https://gitlab.example.com:443/grp/proj/-/merge_requests/3",
			"https://gitlab.example.com:444/grp/proj/-/merge_requests/3"},
		{"GHES with a port", "github",
			map[string]string{"GITHUB_SERVER_URL": "https://ghe.example.com:8443", "GITHUB_REPOSITORY": "acme/widgets"},
			"https://ghe.example.com:8443/acme/widgets/pull/9",
			"https://ghe.example.com/acme/widgets/pull/9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, fc := armTestJiraStage(t, c.forge, c.env)
			writeStageResult(t, st, `{"verb":"opened","url":"`+c.bad+`"}`)
			require.Error(t, st.postJiraStageComment(context.Background(), st.path, ui.New(io.Discard)), "the wrong port is refused")
			writeStageResult(t, st, `{"verb":"opened","url":"`+c.ok+`"}`)
			require.NoError(t, st.postJiraStageComment(context.Background(), st.path, ui.New(io.Discard)))
			body, _ := stageComment(t, fc)
			assert.Contains(t, body, strings.Replace(c.ok, ":443", "", 1), "the normalised URL is posted")
		})
	}
}

func TestJiraStage_InvalidResultIsRefusedAndNothingIsPosted(t *testing.T) {
	for name, content := range map[string]string{
		"result URL on another host is refused":                           `{"verb":"opened","url":"https://other.example.com/acme/widgets/pull/7"}`,
		"result URL for another repository is refused":                    `{"verb":"opened","url":"https://github.com/acme/other/pull/7"}`,
		"result URL for a repository with this name as prefix is refused": `{"verb":"opened","url":"https://github.com/acme/widgets-x/pull/7"}`,
		"result URL that is not a pull request is refused":                `{"verb":"opened","url":"https://github.com/acme/widgets/issues/7"}`,
		"pull request number zero is refused":                             `{"verb":"opened","url":"https://github.com/acme/widgets/pull/0"}`,
		"result URL with a query is refused":                              `{"verb":"opened","url":"https://github.com/acme/widgets/pull/7?x=1"}`,
		"result URL with user info is refused":                            `{"verb":"opened","url":"https://u@github.com/acme/widgets/pull/7"}`,
		"result URL over http is refused":                                 `{"verb":"opened","url":"http://github.com/acme/widgets/pull/7"}`,
		"result URL with an empty port and an empty fragment is refused":  `{"verb":"opened","url":"https://github.com:/acme/widgets/pull/7#"}`,
		"result URL with an empty fragment is refused":                    `{"verb":"opened","url":"https://github.com/acme/widgets/pull/7#"}`,
		"result URL with an empty port is refused":                        `{"verb":"opened","url":"https://github.com:/acme/widgets/pull/7"}`,
		"result URL path with U+017F (folds to s) is refused":             "{\"verb\":\"opened\",\"url\":\"https://github.com/acme/widget\u017f/pull/7\"}",
		"result URL host with U+017F is refused":                          "{\"verb\":\"opened\",\"url\":\"https://github.co\u017f/acme/widgets/pull/7\"}",
		"result URL host with U+212A (Kelvin) is refused":                 "{\"verb\":\"opened\",\"url\":\"https://github.\u212aom/acme/widgets/pull/7\"}",
		"unknown key in the result is refused":                            `{"verb":"opened","url":"https://github.com/acme/widgets/pull/7","key":"PROJ-1"}`,
		"upper-case key VERB is refused":                                  `{"VERB":"opened","url":"https://github.com/acme/widgets/pull/7"}`,
		"mixed-case key Url is refused":                                   `{"verb":"opened","Url":"https://github.com/acme/widgets/pull/7"}`,
		"duplicate verb key is refused":                                   `{"verb":"pushed","verb":"opened","url":"https://github.com/acme/widgets/pull/7"}`,
		"missing url key is refused":                                      `{"verb":"opened"}`,
		"non-string value is refused":                                     `{"verb":"opened","url":7}`,
		"verb other than opened or pushed is refused":                     `{"verb":"merged","url":"https://github.com/acme/widgets/pull/7"}`,
		"data after the JSON object is refused":                           `{"verb":"opened","url":"https://github.com/acme/widgets/pull/7"} {}`,
		"result over 2048 bytes is refused":                               `{"verb":"opened","url":"https://github.com/acme/widgets/pull/7` + strings.Repeat("1", 3000) + `"}`,
		"result that is not JSON is refused":                              "opened https://github.com/acme/widgets/pull/7",
	} {
		t.Run(name, func(t *testing.T) {
			st, fc := armTestJiraStage(t, "github", githubStageEnv())
			path := writeStageResult(t, st, content)
			err := st.postJiraStageComment(context.Background(), path, ui.New(io.Discard))
			require.Error(t, err)
			assert.Empty(t, fc.Comments["PROJ-42"])
			assert.Less(t, len(err.Error()), 400, "the message carries only a bounded excerpt")
		})
	}
	t.Run("result file that is a symlink is refused", func(t *testing.T) {
		st, fc := armTestJiraStage(t, "github", githubStageEnv())
		target := filepath.Join(t.TempDir(), "elsewhere")
		require.NoError(t, os.WriteFile(target, []byte(`{"verb":"opened","url":"https://github.com/acme/widgets/pull/7"}`), 0o600))
		require.NoError(t, os.Remove(st.path))
		require.NoError(t, os.Symlink(target, st.path))
		require.Error(t, st.postJiraStageComment(context.Background(), st.path, ui.New(io.Discard)))
		assert.Empty(t, fc.Comments["PROJ-42"])
	})
	t.Run("result file that is a FIFO is refused", func(t *testing.T) {
		st, fc := armTestJiraStage(t, "github", githubStageEnv())
		require.NoError(t, os.Remove(st.path))
		require.NoError(t, syscall.Mkfifo(st.path, 0o600))
		err := st.postJiraStageComment(context.Background(), st.path, ui.New(io.Discard))
		require.ErrorContains(t, err, "not a regular file")
		assert.Empty(t, fc.Comments["PROJ-42"])
	})
}

func TestJiraStage_EmptyResultPostsNothing(t *testing.T) {
	st, fc := armTestJiraStage(t, "github", githubStageEnv())
	require.NoError(t, st.postJiraStageComment(context.Background(), st.path, ui.New(io.Discard)))
	assert.Empty(t, fc.Comments["PROJ-42"])
}

func TestJiraStage_ResultFileIs0600InA0700DirOutsideForbiddenRoots(t *testing.T) {
	forbidden := t.TempDir()
	prev := jiraStageResultBaseFn
	jiraStageResultBaseFn = func() string { return filepath.Join(forbidden, "under") }
	t.Cleanup(func() { jiraStageResultBaseFn = prev })
	tc, _, err := tracker.NewFakeJiraClientWithFake("https://example.atlassian.net")
	require.NoError(t, err)
	prevClient := jiraStageNewClientFn
	jiraStageNewClientFn = func(*config.TrackingConfig) (*tracker.JiraClient, error) { return tc, nil }
	t.Cleanup(func() { jiraStageNewClientFn = prevClient })

	st, cleanup, err := armJiraStage(stageHarness(), stageOpts(), nil, "github", keepHistoryTrue, jiraEnv(githubStageEnv()), []string{forbidden})
	require.NoError(t, err)
	assert.False(t, pathUnderAny(st.path, []string{forbidden}), "a base under a forbidden root is not used")
	info, err := os.Stat(st.path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.Zero(t, info.Size())
	dinfo, err := os.Stat(filepath.Dir(st.path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dinfo.Mode().Perm())
	cleanup()
	_, err = os.Stat(filepath.Dir(st.path))
	assert.True(t, os.IsNotExist(err))
}

func TestJiraStage_NoOptInMeansNoVariableAndNoPost(t *testing.T) {
	_, _ = armTestJiraStage(t, "github", githubStageEnv())
	t.Setenv(jiraStageResultEnv, "/inherited/path")
	called := false
	keep := func() bool { called = true; return true }

	st, _, err := armJiraStage(&harness.Harness{}, stageOpts(), nil, "github", keep, jiraEnv(githubStageEnv()), nil)
	require.NoError(t, err)
	assert.Nil(t, st, "no opt-in")
	assert.False(t, called, "keep_history is not resolved without the opt-in")
	_, set := os.LookupEnv(jiraStageResultEnv)
	assert.False(t, set, "an inherited value is cleared")
	env := map[string]string{jiraStageResultEnv: harnessEnvExpand(jiraStageResultEnv)}
	dropEmptyOptionalRunnerEnv(env)
	assert.Empty(t, env, "the opt-in resolves empty and is dropped")
	assert.True(t, sandboxDeniedKeys[jiraStageResultEnv])
	assert.True(t, inheritedScriptDenyKeys[jiraStageResultEnv])

	alias := &harness.Harness{Env: &harness.EnvConfig{Runner: map[string]string{"STAGE_FILE": "${" + jiraStageResultEnv + "}"}}}
	st, _, err = armJiraStage(alias, stageOpts(), nil, "github", keep, jiraEnv(githubStageEnv()), nil)
	require.NoError(t, err)
	assert.Nil(t, st, "an alias key with the opt-in value gets nothing")

	notJira := stageOpts()
	notJira.trackerSource = "github"
	st, _, err = armJiraStage(stageHarness(), notJira, nil, "github", keep, jiraEnv(githubStageEnv()), nil)
	require.NoError(t, err)
	assert.Nil(t, st, "not Jira-sourced")

	var cmd exec.Cmd
	(*jiraStage)(nil).attach(&cmd, ui.New(io.Discard))
	assert.Empty(t, cmd.Env, "a nil stage attaches nothing")
}

// TestJiraStage_OptedInJiraRunFailsAtStartWhenSomethingIsMissing: an
// opted-in Jira-sourced run fails at start, not with a warning, when the
// repository or the Jira client is missing.
func TestJiraStage_OptedInJiraRunFailsAtStartWhenSomethingIsMissing(t *testing.T) {
	clearStageJiraSecrets(t)
	_, _, err := armJiraStage(stageHarness(), stageOpts(), parseTestConfig(t, testJiraConfig).IssueTracking(), "github", keepHistoryTrue, jiraEnv(githubStageEnv()), nil)
	require.ErrorContains(t, err, "building the Jira client")
	t.Setenv(jira.TokenEnvVar, "jira-token")
	_, _, err = armJiraStage(stageHarness(), stageOpts(), parseTestConfig(t, testJiraConfig).IssueTracking(), "github", keepHistoryTrue, jiraEnv(map[string]string{}), nil)
	require.ErrorContains(t, err, "GITHUB_REPOSITORY")
	_, _, err = armJiraStage(stageHarness(), stageOpts(), parseTestConfig(t, testJiraConfig).IssueTracking(), "gitlab", keepHistoryTrue, jiraEnv(map[string]string{"CI_PROJECT_URL": "http://gitlab.example.com/g/p"}), nil)
	require.ErrorContains(t, err, "plain https URL")
}

// TestJiraStage_ClientIsBuiltInStaticTokenAndOAuthModes: the client comes
// from the same config and credentials as every other Jira caller.
func TestJiraStage_ClientIsBuiltInStaticTokenAndOAuthModes(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"static token":             {jira.TokenEnvVar: "jira-token"},
		"oauth client credentials": {jira.ClientIDEnvVar: "id", jira.ClientSecretEnvVar: "secret"},
	} {
		t.Run(name, func(t *testing.T) {
			clearStageJiraSecrets(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			st, cleanup, err := armJiraStage(stageHarness(), stageOpts(), parseTestConfig(t, testJiraConfig).IssueTracking(), "github", keepHistoryTrue, jiraEnv(githubStageEnv()), nil)
			require.NoError(t, err)
			defer cleanup()
			assert.NotNil(t, st)
		})
	}
}

// TestJiraStage_ConfigYAMLChangedAfterStartIsNotReread: the post uses what
// was captured at start even when config.yaml changes before it.
func TestJiraStage_ConfigYAMLChangedAfterStartIsNotReread(t *testing.T) {
	clearStageJiraSecrets(t)
	t.Setenv(jira.TokenEnvVar, "jira-token")
	dir := writeJiraPollConfig(t, testJiraConfig)
	reader, err := config.LoadConfig(dir, config.LoadOpts{})
	require.NoError(t, err)
	tc, fc, err := tracker.NewFakeJiraClientWithFake("https://example.atlassian.net")
	require.NoError(t, err)
	var built []string
	prev := jiraStageNewClientFn
	jiraStageNewClientFn = func(tracking *config.TrackingConfig) (*tracker.JiraClient, error) {
		built = append(built, tracking.Jira.BaseURL)
		return tc, nil
	}
	t.Cleanup(func() { jiraStageNewClientFn = prev })
	st, cleanup, err := armJiraStage(stageHarness(), stageOpts(), reader.IssueTracking(), "github", keepHistoryTrue, jiraEnv(githubStageEnv()), nil)
	require.NoError(t, err)
	defer cleanup()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("tracker:\n  jira:\n    base_url: https://other.example.com\n    project: OTHER\n"), 0o644))
	writeStageResult(t, st, `{"verb":"opened","url":"https://github.com/acme/widgets/pull/7"}`)
	require.NoError(t, st.postJiraStageComment(context.Background(), st.path, ui.New(io.Discard)))
	assert.Equal(t, []string{"https://example.atlassian.net"}, built, "the client is built once, at start")
	assert.Len(t, fc.Comments["PROJ-42"], 1, "the post goes to the run's own work item")
}

// TestJiraStage_OnlyThePostScriptGetsTheFileAndThePostFollowsItsExit: the
// post-script launch (childScriptEnv plus attach) gets the variable, a
// pre-script-like child does not, and the comment is posted only after the
// post-script exits; a failed post-script with a valid result still posts.
func TestJiraStage_OnlyThePostScriptGetsTheFileAndThePostFollowsItsExit(t *testing.T) {
	for name, exit := range map[string]string{"post-script succeeds": "0", "post-script fails": "3"} {
		t.Run(name, func(t *testing.T) {
			st, fc := armTestJiraStage(t, "github", githubStageEnv())
			// The opt-in expands empty and is dropped at harness load.
			runnerEnv := map[string]string{jiraStageResultEnv: harnessEnvExpand(jiraStageResultEnv)}
			dropEmptyOptionalRunnerEnv(runnerEnv)

			pre := exec.Command("/bin/sh", "-c", `if [ -n "${`+jiraStageResultEnv+`+set}" ]; then echo set; fi`)
			pre.Env = childScriptEnv(runnerEnv, "")
			out, err := pre.Output()
			require.NoError(t, err)
			assert.Empty(t, strings.TrimSpace(string(out)), "a script other than the post-script gets no variable")

			post := exec.Command("/bin/sh", "-c", `printf '%s\n' '{"verb":"opened","url":"https://github.com/acme/widgets/pull/7"}' > "$`+jiraStageResultEnv+`"; exit `+exit)
			post.Env = childScriptEnv(runnerEnv, "")
			after := st.attach(post, ui.New(io.Discard))
			runErr := post.Run()
			assert.Empty(t, fc.Comments["PROJ-42"], "nothing is posted before the script exits")
			if exit == "0" {
				require.NoError(t, runErr)
			} else {
				require.Error(t, runErr)
			}
			require.NoError(t, after(context.Background()))
			body, _ := stageComment(t, fc)
			assert.Contains(t, body, "opened pull request https://github.com/acme/widgets/pull/7 for PROJ-42.")
		})
	}
}

// TestJiraStage_ResultDirIsNotUnderAnExpandedHostFilesSource: a host_files
// source given as ${RUNNER_TEMP} counts as a forbidden root in its
// expanded form, so the result directory is not created under it.
func TestJiraStage_ResultDirIsNotUnderAnExpandedHostFilesSource(t *testing.T) {
	runnerTemp := t.TempDir()
	t.Setenv("RUNNER_TEMP", runnerTemp)
	h := &harness.Harness{HostFiles: []harness.HostFile{{Src: "${RUNNER_TEMP}", Dest: "/tmp/x"}}}
	roots := jiraStageResultRoots(h, t.TempDir())
	assert.Contains(t, roots, runnerTemp)

	prev := jiraStageResultBaseFn
	jiraStageResultBaseFn = func() string { return runnerTemp }
	t.Cleanup(func() { jiraStageResultBaseFn = prev })
	st := &jiraStage{}
	cleanup, err := st.createResultFile(roots)
	require.NoError(t, err)
	defer cleanup()
	assert.False(t, pathUnderAny(st.path, []string{runnerTemp}), "the result file is not under the host_files source")
}
