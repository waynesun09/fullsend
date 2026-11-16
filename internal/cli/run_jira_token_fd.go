package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fullsend-ai/fullsend/internal/config"
	"github.com/fullsend-ai/fullsend/internal/forge/jira"
	"github.com/fullsend-ai/fullsend/internal/inference/actionsoidc"
	"github.com/fullsend-ai/fullsend/internal/ui"
)

// Jira OIDC token broker for host scripts (tracker.jira.auth: oidc).
// Host scripts never see the runner's ACTIONS_ID_TOKEN_* variables
// (childScriptEnv strips them), and no token is ever written to disk.
// Instead, for each pre- or post-script launch whose harness opts in with
// `FULLSEND_JIRA_OIDC_TOKEN_FD: "${FULLSEND_JIRA_OIDC_TOKEN_FD}"` in
// env.runner, the runner creates a socketpair, passes one end to the
// script (exec.Cmd.ExtraFiles) and names its fd number in
// FULLSEND_JIRA_OIDC_TOKEN_FD. The runner serves the other end until the
// script exits. A request is exactly "token\n" (the current token) or
// "refresh\n" (a fresh one, at most one fetch per
// jiraTokenFDRefreshInterval, otherwise the current token); each is
// answered with "ok <jwt>\n" or "err <code>\n" (bad_request,
// fetch_failed). Requests are served one at a time; a caller must not
// send concurrent requests on one fd. `fullsend issues` in the script uses
// the fd (jira.NewScopedFromEnv).
//
// While a run is in oidc mode, the env.runner reference to
// FULLSEND_JIRA_OIDC_TOKEN_FD expands to a random per-run marker that only
// harnessEnvExpand and harnessEnvLookup know (it is never set in the
// process environment); each pre- or post-script launch replaces it with
// the real fd number. Outside oidc mode the reference expands empty and is
// dropped. A harness cannot forge the marker, and no other site sees it:
// the key is in sandboxDeniedKeys and inheritedScriptDenyKeys, and every
// other script, validation and preflight environment drops a marker value.

// jiraOIDCTokenFDEnv is the variable a host script finds the fd number in.
const jiraOIDCTokenFDEnv = jira.OIDCTokenFDEnvVar

// Broker limits. Variables so tests can shrink them.
var (
	// jiraTokenFDRequestTimeout bounds a request once its first byte has
	// arrived. Waiting for the next request is unbounded until the
	// script exits.
	jiraTokenFDRequestTimeout = 10 * time.Second
	// jiraTokenFDWriteTimeout bounds writing one reply.
	jiraTokenFDWriteTimeout = 10 * time.Second
	// jiraTokenFDRefreshInterval is the least time between two fetches a
	// "refresh" request forces.
	jiraTokenFDRefreshInterval = 5 * time.Second
)

// jiraTokenFDMaxRequest bounds the bytes read for one request.
const jiraTokenFDMaxRequest = 64

// The requests the broker accepts.
const (
	jiraTokenFDRequest = "token\n"
	jiraTokenFDRefresh = "refresh\n"
)

// jiraScriptOIDC is the run's oidc mode for host scripts: the marker
// env.runner references expand to and the token source shared by every
// launch. Both are empty/nil outside oidc mode.
var jiraScriptOIDC struct {
	mu     sync.Mutex
	marker string
	source *jira.OIDCTokenSource
}

func jiraScriptOIDCState() (string, *jira.OIDCTokenSource) {
	jiraScriptOIDC.mu.Lock()
	defer jiraScriptOIDC.mu.Unlock()
	return jiraScriptOIDC.marker, jiraScriptOIDC.source
}

// armJiraScriptOIDC sets up the broker for a run with tracking. Outside
// oidc mode it does nothing. In oidc mode it first validates the
// connection settings and the gateway URL (ValidateConnection,
// jira.CheckOIDCGatewayURL), refuses GitLab CI and any FULLSEND_JIRA_*
// secret, and only then creates the token source for
// tracker.jira.audience, with the runner's OIDC endpoint captured now,
// and a fresh random marker. The returned func disarms it.
func armJiraScriptOIDC(tracking *config.TrackingConfig) (func(), error) {
	disarm := func() {
		jiraScriptOIDC.mu.Lock()
		jiraScriptOIDC.marker, jiraScriptOIDC.source = "", nil
		jiraScriptOIDC.mu.Unlock()
	}
	// fullsend run itself never uses a broker fd from its own environment.
	_ = os.Unsetenv(jiraOIDCTokenFDEnv)
	if tracking == nil || !tracking.Jira.IsOIDC() {
		return disarm, nil
	}
	j := tracking.Jira
	if err := j.ValidateConnection(); err != nil {
		return disarm, err
	}
	if err := jira.CheckOIDCGatewayURL(j.APIURL, j.BaseURL); err != nil {
		return disarm, err
	}
	if err := jiraOIDCRunPreflight(os.Getenv); err != nil {
		return disarm, err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return disarm, fmt.Errorf("arming the Jira OIDC token broker: %w", err)
	}
	marker := "fullsend-jira-oidc-token-fd-pending-" + hex.EncodeToString(b[:])
	audience := strings.TrimSpace(j.Audience)
	reqURL, reqToken := jiraOIDCEndpoint()
	source := jira.NewOIDCTokenSource(audience, func(ctx context.Context) (*actionsoidc.Assertion, error) {
		return jiraOIDCFetchAtFn(ctx, audience, reqURL, reqToken)
	})
	jiraScriptOIDC.mu.Lock()
	jiraScriptOIDC.marker, jiraScriptOIDC.source = marker, source
	jiraScriptOIDC.mu.Unlock()
	return disarm, nil
}

// jiraScriptOIDCArmed reports whether the run is in oidc mode for host
// scripts.
func jiraScriptOIDCArmed() bool {
	marker, _ := jiraScriptOIDCState()
	return marker != ""
}

// jiraTokenFDHarnessValue is what an env.runner reference to
// FULLSEND_JIRA_OIDC_TOKEN_FD expands to: the armed marker, or empty.
func jiraTokenFDHarnessValue() string {
	marker, _ := jiraScriptOIDCState()
	return marker
}

// isJiraTokenFDMarker reports whether v is the armed marker.
func isJiraTokenFDMarker(v string) bool {
	marker, _ := jiraScriptOIDCState()
	return marker != "" && v == marker
}

// jiraTokenFDKeys returns FULLSEND_JIRA_OIDC_TOKEN_FD when runnerEnv
// holds the armed marker under exactly that key. Another key with the
// marker value gets nothing (childScriptEnv drops it).
func jiraTokenFDKeys(runnerEnv map[string]string) []string {
	if isJiraTokenFDMarker(runnerEnv[jiraOIDCTokenFDEnv]) {
		return []string{jiraOIDCTokenFDEnv}
	}
	return nil
}

// attachJiraTokenBroker gives cmd, a host script whose Env is already
// set, the broker when its harness runnerEnv opted in: it appends the
// script end of a new socketpair to cmd.ExtraFiles, the fd number to
// cmd.Env, and starts serving the other end. Outside oidc mode, or
// without the opt-in, it changes nothing. stop cancels a fetch in flight,
// closes both ends and waits for the server; the caller defers it right
// after a nil error, so the broker ends when the script exits, however it
// exits.
func attachJiraTokenBroker(cmd *exec.Cmd, runnerEnv map[string]string, printer *ui.Printer) (stop func(), err error) {
	noop := func() {}
	keys := jiraTokenFDKeys(runnerEnv)
	_, source := jiraScriptOIDCState()
	if len(keys) == 0 || source == nil {
		return noop, nil
	}
	core, script, err := jiraTokenSocketpair()
	if err != nil {
		return noop, fmt.Errorf("creating the Jira OIDC token broker: %w", err)
	}
	fd := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(cmd.ExtraFiles, script)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+strconv.Itoa(fd))
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		serveJiraTokenBroker(ctx, core, source, printer)
	}()
	return func() {
		cancel()
		_ = core.Close()
		_ = script.Close()
		wg.Wait()
	}, nil
}

// jiraTokenSocketpair returns the runner's end as a net.Conn and the
// script's end as a file, both close-on-exec: exec.Cmd duplicates the
// script end into the one child it is passed to, and no other child
// inherits either end.
func jiraTokenSocketpair() (net.Conn, *os.File, error) {
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, err
	}
	coreFile := os.NewFile(uintptr(fds[0]), "jira-oidc-token-broker")
	conn, err := net.FileConn(coreFile)
	_ = coreFile.Close()
	if err != nil {
		_ = syscall.Close(fds[1])
		return nil, nil, err
	}
	return conn, os.NewFile(uintptr(fds[1]), "jira-oidc-token-fd"), nil
}

// serveJiraTokenBroker answers requests on conn until it fails, is
// closed, or ctx ends. A request that is not exactly "token\n" or
// "refresh\n" (within jiraTokenFDMaxRequest bytes and
// jiraTokenFDRequestTimeout of its first byte) gets "err bad_request" and
// ends the broker. A fetch failure gets "err fetch_failed" and one log
// line without the token. Each token is registered for redaction and the
// Actions mask before it is written.
func serveJiraTokenBroker(ctx context.Context, conn net.Conn, source *jira.OIDCTokenSource, printer *ui.Printer) {
	defer conn.Close()
	// registered is the last token registered and masked, so a cached
	// token is masked once, not on every request.
	var registered string
	var lastRefresh time.Time
	for {
		req, err := readJiraTokenRequest(conn)
		if errors.Is(err, errJiraTokenBrokerClosed) {
			return
		}
		if err != nil || (req != jiraTokenFDRequest && req != jiraTokenFDRefresh) {
			writeJiraTokenReply(conn, "err bad_request\n")
			return
		}
		fctx, cancel := context.WithTimeout(ctx, gatewayFetchTimeout)
		var tok string
		if req == jiraTokenFDRefresh && time.Since(lastRefresh) >= jiraTokenFDRefreshInterval {
			lastRefresh = time.Now()
			tok, err = source.Refresh(fctx)
		} else {
			tok, err = source.Token(fctx)
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil && tok != registered {
			if err = registerBrokerToken(tok, os.Getenv("GITHUB_ACTIONS") == "true"); err == nil {
				registered = tok
			}
		}
		if err != nil {
			printer.StepWarn("Jira OIDC token request from the host script failed: " + err.Error())
			if !writeJiraTokenReply(conn, "err fetch_failed\n") {
				return
			}
			continue
		}
		if !writeJiraTokenReply(conn, "ok "+tok+"\n") {
			return
		}
	}
}

// jiraBrokerMaskOut is where the broker writes its Actions mask lines: the
// run's stderr, which the script shares. Override in tests.
var jiraBrokerMaskOut io.Writer = os.Stderr

// registerBrokerToken is registerJiraToken for a token the broker hands
// out, with the mask line on a line of its own: the script may have left
// a partial line on the shared stderr, which would otherwise swallow the
// ::add-mask:: command.
func registerBrokerToken(tok string, maskActions bool) error {
	if err := registerJiraToken(tok, false); err != nil {
		return err
	}
	if maskActions {
		fmt.Fprintf(jiraBrokerMaskOut, "\n::add-mask::%s\n", actionsCommandEscaper.Replace(tok))
	}
	return nil
}

// errJiraTokenBrokerClosed is the peer closing (or the broker stopping)
// between requests.
var errJiraTokenBrokerClosed = errors.New("jira token broker closed")

// readJiraTokenRequest reads one request, byte by byte so a pipelined
// next request is never consumed. The wait for its first byte is
// unbounded; after it the request must end within
// jiraTokenFDRequestTimeout and jiraTokenFDMaxRequest bytes.
func readJiraTokenRequest(conn net.Conn) (string, error) {
	var buf [jiraTokenFDMaxRequest]byte
	_ = conn.SetReadDeadline(time.Time{})
	if _, err := conn.Read(buf[:1]); err != nil {
		return "", errJiraTokenBrokerClosed
	}
	n := 1
	_ = conn.SetReadDeadline(time.Now().Add(jiraTokenFDRequestTimeout))
	for buf[n-1] != '\n' {
		if n == len(buf) {
			return "", errors.New("request too long")
		}
		m, err := conn.Read(buf[n : n+1])
		if err != nil {
			return "", err
		}
		n += m
	}
	return string(buf[:n]), nil
}

// writeJiraTokenReply writes one reply within jiraTokenFDWriteTimeout and
// reports whether it was written.
func writeJiraTokenReply(conn net.Conn, reply string) bool {
	_ = conn.SetWriteDeadline(time.Now().Add(jiraTokenFDWriteTimeout))
	_, err := conn.Write([]byte(reply))
	return err == nil
}
