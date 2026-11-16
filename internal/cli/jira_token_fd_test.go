package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/config"
	"github.com/fullsend-ai/fullsend/internal/forge/jira"
	"github.com/fullsend-ai/fullsend/internal/harness"
	"github.com/fullsend-ai/fullsend/internal/inference/actionsoidc"
	"github.com/fullsend-ai/fullsend/internal/tracker"
	"github.com/fullsend-ai/fullsend/internal/ui"
)

// armTestJiraBroker arms oidc mode for host scripts with a fetch that
// issues tokens for oidcJiraAudience with the given lifetime.
func armTestJiraBroker(t *testing.T, lifetime time.Duration) (*atomic.Int32, string) {
	t.Helper()
	clearJiraSecrets(t)
	t.Setenv("GITHUB_ACTIONS", "")
	var fetches atomic.Int32
	prev := jiraOIDCFetchAtFn
	jiraOIDCFetchAtFn = func(_ context.Context, audience, _, _ string) (*actionsoidc.Assertion, error) {
		n := fetches.Add(1)
		return testOIDCAssertion(audience, lifetime, strings.Repeat("n", int(n))), nil
	}
	restore, err := armJiraScriptOIDC(oidcTracking(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		restore()
		jiraOIDCFetchAtFn = prev
	})
	marker, _ := jiraScriptOIDCState()
	require.NotEmpty(t, marker)
	return &fetches, marker
}

// brokerConn returns an in-process connection on the script end the
// broker attached to cmd.
func brokerConn(t *testing.T, cmd *exec.Cmd) net.Conn {
	t.Helper()
	require.Len(t, cmd.ExtraFiles, 1)
	c, err := net.FileConn(cmd.ExtraFiles[0])
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestJiraTokenBroker_ScriptReadsTwoTokensAcrossALifetime(t *testing.T) {
	fetches, marker := armTestJiraBroker(t, 3*time.Second)
	script := `for i in 1 2; do
  printf 'token\n' >&"$FULLSEND_JIRA_OIDC_TOKEN_FD"
  IFS= read -r line <&"$FULLSEND_JIRA_OIDC_TOKEN_FD" || exit 3
  printf '%s\n' "$line"
  if [ "$i" = 1 ]; then sleep 2; fi
done`
	cmd := exec.Command("/bin/sh", "-c", script)
	runnerEnv := map[string]string{jiraOIDCTokenFDEnv: marker}
	cmd.Env = childScriptEnv(runnerEnv, "")
	var out bytes.Buffer
	cmd.Stdout = &out
	stop, err := attachJiraTokenBroker(cmd, runnerEnv, ui.New(io.Discard))
	require.NoError(t, err)
	assert.Contains(t, cmd.Env, jiraOIDCTokenFDEnv+"=3")
	require.NoError(t, cmd.Run())
	stop()

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 2)
	var toks []string
	for _, l := range lines {
		tok, ok := strings.CutPrefix(l, "ok ")
		require.True(t, ok, "reply %q", l)
		toks = append(toks, tok)
	}
	assert.NotEqual(t, toks[0], toks[1], "the second request, after the first token neared exp, gets a fresh token")
	assert.Equal(t, int32(2), fetches.Load())
	assert.True(t, testOIDCTokenValid(toks[1], oidcJiraAudience), "the fresh token is valid")
}

func TestJiraTokenBroker_MalformedOversizedOrStalledRequestEndsTheBroker(t *testing.T) {
	_, marker := armTestJiraBroker(t, time.Minute)
	prev := jiraTokenFDRequestTimeout
	jiraTokenFDRequestTimeout = 100 * time.Millisecond
	t.Cleanup(func() { jiraTokenFDRequestTimeout = prev })

	for name, req := range map[string]string{
		"malformed": "tokn\n",
		"oversized": strings.Repeat("a", 100),
		"stalled":   "tok",
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("/bin/true")
			runnerEnv := map[string]string{jiraOIDCTokenFDEnv: marker}
			stop, err := attachJiraTokenBroker(cmd, runnerEnv, ui.New(io.Discard))
			require.NoError(t, err)
			c := brokerConn(t, cmd)
			_, err = c.Write([]byte(req))
			require.NoError(t, err)
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			reply, err := io.ReadAll(c)
			require.NoError(t, err, "the broker closes its end")
			assert.Equal(t, "err bad_request\n", string(reply))
			done := make(chan struct{})
			go func() { stop(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("stop hung")
			}
		})
	}
}

func TestJiraTokenBroker_ScriptExitClosesTheBroker(t *testing.T) {
	_, marker := armTestJiraBroker(t, time.Minute)
	cmd := exec.Command("/bin/sh", "-c", "exit 4")
	runnerEnv := map[string]string{jiraOIDCTokenFDEnv: marker}
	cmd.Env = childScriptEnv(runnerEnv, "")
	stop, err := attachJiraTokenBroker(cmd, runnerEnv, ui.New(io.Discard))
	require.NoError(t, err)
	require.Error(t, cmd.Run(), "a failing script")
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the broker outlived the script")
	}
}

func TestJiraTokenBroker_FetchFailure(t *testing.T) {
	_, marker := armTestJiraBroker(t, time.Minute)
	jiraOIDCFetchAtFn = func(context.Context, string, string, string) (*actionsoidc.Assertion, error) {
		return nil, errors.New("OIDC endpoint returned 500")
	}
	cmd := exec.Command("/bin/true")
	var log syncBuffer
	stop, err := attachJiraTokenBroker(cmd, map[string]string{jiraOIDCTokenFDEnv: marker}, ui.New(&log))
	require.NoError(t, err)
	defer stop()
	c := brokerConn(t, cmd)
	_, err = c.Write([]byte("token\n"))
	require.NoError(t, err)
	line, err := bufio.NewReader(c).ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "err fetch_failed\n", line)
	assert.Contains(t, log.String(), "OIDC endpoint returned 500")
	assert.NotContains(t, log.String(), "eyJ")
}

func TestJiraTokenBroker_FDOnlyWithOptInInOIDCModeNeverInSandboxOrOtherScripts(t *testing.T) {
	t.Run("no fd without the env.runner opt-in", func(t *testing.T) {
		armTestJiraBroker(t, time.Minute)
		cmd := exec.Command("/bin/true")
		stop, err := attachJiraTokenBroker(cmd, map[string]string{"OTHER": "x"}, ui.New(io.Discard))
		require.NoError(t, err)
		stop()
		assert.Empty(t, cmd.ExtraFiles)
		assert.Empty(t, cmd.Env)
	})
	t.Run("opt-in expands to the marker and is optional", func(t *testing.T) {
		_, marker := armTestJiraBroker(t, time.Minute)
		v, ok := harnessEnvLookup(jiraOIDCTokenFDEnv)
		assert.True(t, ok)
		assert.Equal(t, marker, v)
		assert.True(t, isOptionalHarnessEnvKey(jira.TokenEnvVar), "FULLSEND_JIRA_TOKEN is optional in oidc mode")
		// Other script children never see the marker.
		assert.NotContains(t, strings.Join(childScriptEnv(map[string]string{jiraOIDCTokenFDEnv: marker}, ""), "\n"), marker)
		assert.NotContains(t, strings.Join(inheritedScriptEnv(), "\n"), marker)
		_, set := os.LookupEnv(jiraOIDCTokenFDEnv)
		assert.False(t, set, "the marker is never in the process environment")
		pre := preflightEnv(map[string]string{jiraOIDCTokenFDEnv: marker, "ALIAS": marker, "KEEP": "1"}, t.TempDir())
		assert.NotContains(t, pre, jiraOIDCTokenFDEnv)
		assert.NotContains(t, pre, "ALIAS")
		assert.Equal(t, "1", pre["KEEP"])
		assert.NotContains(t, strings.Join(validationEnv(&harness.Harness{RunnerEnv: map[string]string{jiraOIDCTokenFDEnv: marker, "KEEP": "1"}}, "", t.TempDir()), "\n"), marker)
	})
	t.Run("the fd variable never expands into env.sandbox or host_files", func(t *testing.T) {
		armTestJiraBroker(t, time.Minute)
		assert.True(t, sandboxDeniedKeys[jiraOIDCTokenFDEnv])
		assert.Empty(t, expandSandboxEnvValue("${"+jiraOIDCTokenFDEnv+"}", harnessEnvExpand))
	})
	t.Run("outside oidc mode the variable is dropped and no fd is passed", func(t *testing.T) {
		clearJiraSecrets(t)
		t.Setenv(jiraOIDCTokenFDEnv, "7")
		restore, err := armJiraScriptOIDC(parseTestConfig(t, testJiraConfig).IssueTracking())
		require.NoError(t, err)
		defer restore()
		assert.Empty(t, harnessEnvExpand(jiraOIDCTokenFDEnv), "an inherited value is not passed on")
		v, ok := harnessEnvLookup(jiraOIDCTokenFDEnv)
		assert.True(t, ok)
		assert.Empty(t, v)
		env := map[string]string{jiraOIDCTokenFDEnv: harnessEnvExpand(jiraOIDCTokenFDEnv)}
		dropEmptyOptionalRunnerEnv(env)
		assert.Empty(t, env, "the opt-in resolves empty and is dropped")
		assert.False(t, isOptionalHarnessEnvKey(jira.TokenEnvVar))
		cmd := exec.Command("/bin/true")
		stop, err := attachJiraTokenBroker(cmd, map[string]string{jiraOIDCTokenFDEnv: "7"}, ui.New(io.Discard))
		require.NoError(t, err)
		stop()
		assert.Empty(t, cmd.ExtraFiles)
	})
	t.Run("arming is refused next to a static credential", func(t *testing.T) {
		clearJiraSecrets(t)
		t.Setenv(jira.ClientIDEnvVar, "id")
		restore, err := armJiraScriptOIDC(oidcTracking(t))
		defer restore()
		require.Error(t, err)
		assert.Contains(t, err.Error(), jira.ClientIDEnvVar)
	})
}

// TestJiraTokenBroker_HookLikeChildDoesNotInheritTheBrokerFD: while a
// script's broker is attached, a child started the way fullsend starts
// children that are not the opted-in script (childScriptEnv, exec.Cmd
// without ExtraFiles) sees neither socket end and no
// FULLSEND_JIRA_OIDC_TOKEN_FD. Both ends are also close-on-exec.
func TestJiraTokenBroker_HookLikeChildDoesNotInheritTheBrokerFD(t *testing.T) {
	_, marker := armTestJiraBroker(t, time.Minute)
	runnerEnv := map[string]string{jiraOIDCTokenFDEnv: marker}
	cmd := exec.Command("/bin/true")
	stop, err := attachJiraTokenBroker(cmd, runnerEnv, ui.New(io.Discard))
	require.NoError(t, err)
	scriptFD := cmd.ExtraFiles[0].Fd()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, scriptFD, syscall.F_GETFD, 0)
	require.Zero(t, errno)
	assert.NotZero(t, flags&syscall.FD_CLOEXEC, "the script end is close-on-exec")

	// The child probes descriptors 3..scriptFD+64 (both broker ends were
	// the lowest free ones at attach time) and reports only whether the
	// variable is set, never the environment itself. A baseline child run
	// before the broker existed shows what this process already passes on.
	limit := strconv.Itoa(int(scriptFD) + 64)
	probe := `i=3; while [ "$i" -le "$1" ]; do (: <&"$i") 2>/dev/null && echo "open fd $i"; i=$((i+1)); done; ` +
		`if [ -n "${` + jiraOIDCTokenFDEnv + `+set}" ]; then echo "variable set"; fi`
	run := func() string {
		child := exec.Command("/bin/sh", "-c", probe, "sh", limit)
		child.Env = childScriptEnv(runnerEnv, "")
		out, err := child.Output()
		require.NoError(t, err)
		return string(out)
	}
	stop()
	baseline := run()

	cmd = exec.Command("/bin/true")
	stop, err = attachJiraTokenBroker(cmd, runnerEnv, ui.New(io.Discard))
	require.NoError(t, err)
	defer stop()
	require.Equal(t, scriptFD, cmd.ExtraFiles[0].Fd(), "the broker reuses the same descriptor numbers")
	withBroker := run()
	assert.Equal(t, baseline, withBroker, "the child sees neither broker socket end")
	assert.NotContains(t, withBroker, "open fd "+strconv.Itoa(int(scriptFD))+"\n")
	assert.NotContains(t, withBroker, "variable set", "the variable is absent from the child's environment")
}

func TestFullsendIssues_UsesTheBrokerFD(t *testing.T) {
	_, marker := armTestJiraBroker(t, 2*time.Second)
	g := newFakeJiraGateway(t)
	setJiraScopedTestOptions(t, jira.WithHTTPClient(g.client()))

	cmd := exec.Command("/bin/true")
	stop, err := attachJiraTokenBroker(cmd, map[string]string{jiraOIDCTokenFDEnv: marker}, ui.New(io.Discard))
	require.NoError(t, err)
	defer stop()
	// In the script, `fullsend issues` sees the fd and no ACTIONS_ID_TOKEN_*.
	// The client owns (and closes) the fd it is given: hand it a duplicate,
	// as the child's fd 3 is a duplicate of the script end.
	dup, err := syscall.Dup(int(cmd.ExtraFiles[0].Fd()))
	require.NoError(t, err)
	t.Setenv(jiraOIDCTokenFDEnv, strconv.Itoa(dup))

	tc, err := newConfiguredTrackerClient(trackerJira, "", "", parseTestConfig(t, oidcJiraConfig))
	require.NoError(t, err)
	jc := tc.(*tracker.JiraClient)
	_, err = jc.AuthenticatedUser(context.Background())
	require.NoError(t, err)
	time.Sleep(2 * time.Second)
	_, err = jc.AuthenticatedUser(context.Background())
	require.NoError(t, err, "the second call requests a fresh token from the broker")
	seen := g.seen()
	require.GreaterOrEqual(t, len(seen), 2)
	assert.NotEqual(t, seen[0], seen[len(seen)-1])

	t.Setenv(jira.TokenEnvVar, "static")
	_, err = newConfiguredTrackerClient(trackerJira, "", "", parseTestConfig(t, oidcJiraConfig))
	require.Error(t, err)
	assert.Contains(t, err.Error(), jira.TokenEnvVar)
}

func TestResolveJiraRunEnv_OIDCPointsAtTheGateway(t *testing.T) {
	var got jira.ScopedConfig
	env, err := resolveJiraRunEnv(context.Background(), "jira", oidcTracking(t), false, func(_ context.Context, cfg jira.ScopedConfig) (string, error) {
		got = cfg
		return cfg.APIURL + "/ex/jira/" + testJiraCloudID, nil
	})
	require.NoError(t, err)
	assert.Equal(t, oidcJiraGateway, got.APIURL)
	assert.Equal(t, oidcJiraGateway+"/ex/jira/"+testJiraCloudID, env[envJiraAPIURL])
	assert.NotContains(t, env[envJiraAPIURL], "atlassian.com")
}

func TestJiraTokenBroker_RefreshIsRateLimited(t *testing.T) {
	fetches, marker := armTestJiraBroker(t, time.Minute)
	cmd := exec.Command("/bin/true")
	stop, err := attachJiraTokenBroker(cmd, map[string]string{jiraOIDCTokenFDEnv: marker}, ui.New(io.Discard))
	require.NoError(t, err)
	defer stop()
	c := brokerConn(t, cmd)
	r := bufio.NewReader(c)
	ask := func(req string) string {
		_, err := c.Write([]byte(req))
		require.NoError(t, err)
		line, err := r.ReadString('\n')
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(line, "ok "), line)
		return line
	}
	first := ask("token\n")
	assert.Equal(t, first, ask("token\n"), "token returns the cached token")
	second := ask("refresh\n")
	assert.NotEqual(t, first, second, "refresh forces a fetch")
	assert.Equal(t, second, ask("refresh\n"), "a second refresh within the interval returns the current token")
	assert.Equal(t, int32(2), fetches.Load())
}

func TestJiraTokenBroker_StopCancelsAFetchInFlight(t *testing.T) {
	_, marker := armTestJiraBroker(t, time.Minute)
	started := make(chan struct{})
	jiraOIDCFetchAtFn = func(ctx context.Context, _, _, _ string) (*actionsoidc.Assertion, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	cmd := exec.Command("/bin/true")
	stop, err := attachJiraTokenBroker(cmd, map[string]string{jiraOIDCTokenFDEnv: marker}, ui.New(io.Discard))
	require.NoError(t, err)
	c := brokerConn(t, cmd)
	_, err = c.Write([]byte("token\n"))
	require.NoError(t, err)
	<-started
	start := time.Now()
	stop()
	assert.Less(t, time.Since(start), 2*time.Second, "stop does not wait out the fetch timeout")
}

func TestJiraScriptOIDC_NoEndpointGivesTheIDTokenError(t *testing.T) {
	armTestJiraBroker(t, time.Minute)
	setJiraScopedTestOptions(t)
	// Inside a run, a Jira client in fullsend run itself has no broker fd
	// and no ACTIONS_ID_TOKEN_*: the id-token: write error, not an fd one.
	_, err := newJiraTrackerClientFromConfig(oidcTracking(t))
	require.ErrorIs(t, err, jira.ErrOIDCEndpointMissing)
	assert.Contains(t, err.Error(), "id-token: write")
}

func TestArmJiraScriptOIDC_ValidatesFirst(t *testing.T) {
	clearJiraSecrets(t)
	bad := *oidcTracking(t).Jira
	bad.APIURL = "https://api.atlassian\u3002com"
	restore, err := armJiraScriptOIDC(&config.TrackingConfig{Jira: &bad})
	restore()
	require.ErrorContains(t, err, "ASCII DNS name")
	assert.False(t, jiraScriptOIDCArmed())

	t.Setenv("GITLAB_CI", "true")
	restore, err = armJiraScriptOIDC(oidcTracking(t))
	restore()
	require.ErrorContains(t, err, "not supported on GitLab CI yet")
	assert.False(t, jiraScriptOIDCArmed())
}

func TestFullsendIssues_RetryAfter401SendsRefresh(t *testing.T) {
	fetches, marker := armTestJiraBroker(t, time.Minute)
	g := newFakeJiraGateway(t)
	setJiraScopedTestOptions(t, jira.WithHTTPClient(g.client()))
	cmd := exec.Command("/bin/true")
	stop, err := attachJiraTokenBroker(cmd, map[string]string{jiraOIDCTokenFDEnv: marker}, ui.New(io.Discard))
	require.NoError(t, err)
	defer stop()
	dup, err := syscall.Dup(int(cmd.ExtraFiles[0].Fd()))
	require.NoError(t, err)
	t.Setenv(jiraOIDCTokenFDEnv, strconv.Itoa(dup))

	tc, err := newConfiguredTrackerClient(trackerJira, "", "", parseTestConfig(t, oidcJiraConfig))
	require.NoError(t, err)
	jc := tc.(*tracker.JiraClient)
	_, err = jc.AuthenticatedUser(context.Background())
	require.NoError(t, err)
	g.refuse(g.seen()[0])
	_, err = jc.AuthenticatedUser(context.Background())
	require.NoError(t, err, "the retry after the 401 asks the broker to refresh")
	assert.Equal(t, int32(2), fetches.Load())
}

// TestJiraTokenBroker_AliasKeyGetsNoFD: only the exact key
// FULLSEND_JIRA_OIDC_TOKEN_FD opts in; another key whose value is the
// reference gets nothing, and fullsend run drops an inherited value.
func TestJiraTokenBroker_AliasKeyGetsNoFD(t *testing.T) {
	_, marker := armTestJiraBroker(t, time.Minute)
	cmd := exec.Command("/bin/true")
	stop, err := attachJiraTokenBroker(cmd, map[string]string{"MY_JIRA_FD": marker}, ui.New(io.Discard))
	require.NoError(t, err)
	stop()
	assert.Empty(t, cmd.ExtraFiles)
	assert.Empty(t, cmd.Env)
	assert.NotContains(t, strings.Join(childScriptEnv(map[string]string{"MY_JIRA_FD": marker}, ""), "\n"), marker)

	t.Setenv(jiraOIDCTokenFDEnv, "9")
	restore, err := armJiraScriptOIDC(oidcTracking(t))
	require.NoError(t, err)
	defer restore()
	_, set := os.LookupEnv(jiraOIDCTokenFDEnv)
	assert.False(t, set, "fullsend run unsets an inherited broker fd variable")
}

// TestJiraTokenBroker_MaskLineStartsALine: the broker writes the Actions
// mask command after a newline, so a partial line the script left on the
// shared stderr cannot swallow it.
func TestJiraTokenBroker_MaskLineStartsALine(t *testing.T) {
	_, marker := armTestJiraBroker(t, time.Minute)
	t.Setenv("GITHUB_ACTIONS", "true")
	var masks syncBuffer
	prev := jiraBrokerMaskOut
	jiraBrokerMaskOut = &masks
	t.Cleanup(func() { jiraBrokerMaskOut = prev })

	cmd := exec.Command("/bin/true")
	stop, err := attachJiraTokenBroker(cmd, map[string]string{jiraOIDCTokenFDEnv: marker}, ui.New(io.Discard))
	require.NoError(t, err)
	defer stop()
	c := brokerConn(t, cmd)
	_, err = c.Write([]byte("token\n"))
	require.NoError(t, err)
	line, err := bufio.NewReader(c).ReadString('\n')
	require.NoError(t, err)
	tok := strings.TrimSuffix(strings.TrimPrefix(line, "ok "), "\n")
	assert.Equal(t, "\n::add-mask::"+tok+"\n", masks.String())
}
