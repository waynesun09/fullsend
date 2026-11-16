package jira

import (
	"bufio"
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// brokerPair returns the client's fd number and a fake broker that answers
// each request with the next of replies, recording the requests in got
// (when non-nil). The broker closes after the last reply.
func brokerPair(t *testing.T, got *[]string, replies ...string) string {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	require.NoError(t, err)
	f := os.NewFile(uintptr(fds[0]), "broker")
	conn, err := net.FileConn(f)
	require.NoError(t, err)
	_ = f.Close()
	// The client takes ownership of the fd it is given and closes it, so
	// it gets a duplicate; the test closes only its own descriptors.
	dup, err := syscall.Dup(fds[1])
	require.NoError(t, err)
	_ = syscall.Close(fds[1])
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		r := bufio.NewReader(conn)
		defer conn.Close()
		for _, reply := range replies {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if got != nil {
				*got = append(*got, line)
			}
			_, _ = conn.Write([]byte(reply))
		}
	}()
	return strconv.Itoa(dup)
}

func fdFetch(t *testing.T, fd string, request string) OIDCFetchFunc {
	t.Helper()
	n, ok := parseOIDCTokenFD(fd)
	require.True(t, ok)
	return (&oidcTokenFDClient{fd: n}).fetch(request)
}

func TestOIDCTokenFDClient_ErrorRepliesFailAndABrokenConnectionIsNotReused(t *testing.T) {
	ctx := context.Background()
	jwt := oidcTestAssertion(oidcTestAudience, time.Minute, "x").Value

	fetch := fdFetch(t, brokerPair(t, nil, "ok "+jwt+"\n", "err fetch_failed\n", "err Bad Code\n", "garbage\n"), "token\n")
	a, err := fetch(ctx)
	require.NoError(t, err)
	assert.Equal(t, jwt, a.Value)
	assert.Equal(t, time.Minute, a.Lifetime())
	_, err = fetch(ctx)
	require.ErrorContains(t, err, "answered fetch_failed")
	_, err = fetch(ctx)
	require.ErrorContains(t, err, "answered unknown")
	_, err = fetch(ctx)
	require.ErrorContains(t, err, "malformed")
	_, err = fetch(ctx)
	require.ErrorContains(t, err, "Jira OIDC token broker", "the broker closed")
	_, err = fetch(ctx)
	require.ErrorContains(t, err, "not reused", "a failed connection is not used again")

	fetch = fdFetch(t, brokerPair(t, nil, "ok not-a-jwt\n", "ok "+strings.Repeat("a", maxOIDCTokenFDReply+10)+"\n"), "token\n")
	_, err = fetch(ctx)
	require.ErrorContains(t, err, "broker's token")
	_, err = fetch(ctx)
	require.ErrorContains(t, err, "too long")

	for _, bad := range []string{"x", "2", "-1", "", "fullsend-jira-oidc-token-fd-pending-ab"} {
		_, ok := parseOIDCTokenFD(bad)
		assert.False(t, ok, bad)
	}
}

func TestNewScopedFromEnv_TokenFDRequestsPerNeedAndRefreshesAfter401(t *testing.T) {
	g := newOIDCGateway(t)
	a := oidcTestAssertion(oidcTestAudience, time.Minute, "fd1")
	b := oidcTestAssertion(oidcTestAudience, time.Minute, "fd2")
	var got []string
	fd := brokerPair(t, &got, "ok "+a.Value+"\n", "ok "+a.Value+"\n", "ok "+b.Value+"\n")
	env := map[string]string{OIDCTokenFDEnvVar: fd}
	c, err := NewScopedFromEnv(oidcScopedConfig(), func(k string) string { return env[k] }, WithHTTPClient(g.client()))
	require.NoError(t, err)
	assert.True(t, c.oidc.perRequest, "a token is requested per need")
	_, err = c.GetMyself(context.Background())
	require.NoError(t, err)
	g.refuse(a.Value)
	_, err = c.GetMyself(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"token\n", "token\n", "refresh\n"}, got, "the retry after a 401 sends refresh")

	env[TokenEnvVar] = "static"
	_, err = NewScopedFromEnv(oidcScopedConfig(), func(k string) string { return env[k] }, WithHTTPClient(g.client()))
	require.ErrorContains(t, err, TokenEnvVar)
	delete(env, TokenEnvVar)
	for _, v := range []string{"1", "marker-value"} {
		env[OIDCTokenFDEnvVar] = v
		_, err = NewScopedFromEnv(oidcScopedConfig(), func(k string) string { return env[k] }, WithHTTPClient(g.client()))
		require.ErrorIs(t, err, ErrOIDCEndpointMissing, "an fd value that is not a number of at least 3 counts as unset")
	}
}

func TestOIDCTokenSource_Refresh(t *testing.T) {
	var calls atomic.Int32
	src := NewOIDCTokenSource(oidcTestAudience, countingFetch(oidcTestAudience, time.Minute, &calls))
	ctx := context.Background()
	first, err := src.Token(ctx)
	require.NoError(t, err)
	again, err := src.Token(ctx)
	require.NoError(t, err)
	assert.Equal(t, first, again)
	fresh, err := src.Refresh(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, first, fresh)
	assert.Equal(t, int32(2), calls.Load())
}

// TestOIDCTokenFDClient_NonSocketFDIsRefusedAndLeftOpen: a pipe named as
// the broker fd is refused before it is taken over, and stays open.
func TestOIDCTokenFDClient_NonSocketFDIsRefusedAndLeftOpen(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	fd := int(r.Fd())
	_, err = (&oidcTokenFDClient{fd: fd}).fetch("token\n")(context.Background())
	require.ErrorContains(t, err, "is not a socket")
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	assert.Zero(t, errno, "the pipe fd is still open")
}
