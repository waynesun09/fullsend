package jira

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fullsend-ai/fullsend/internal/inference/actionsoidc"
)

// OIDCTokenFDEnvVar names the fd of the runner's Jira OIDC token broker in
// a host script (tracker.jira.auth: oidc). `fullsend run` sets it only for
// a pre- or post-script whose harness opts in; the script holds the
// socket, never a token file. A request is "token\n" (the current token)
// or "refresh\n" (a fresh one, rate-limited by the runner), answered with
// "ok <jwt>\n" or "err <code>\n". Requests must not be sent concurrently
// on one fd.
const OIDCTokenFDEnvVar = "FULLSEND_JIRA_OIDC_TOKEN_FD"

// OIDCTokenSource is the oidc mode's token cache for a caller that hands
// the token on rather than calling Jira itself: `fullsend run` serves it
// to host scripts. It caches a token until exp minus min(60s,
// lifetime/4) and serializes fetches.
type OIDCTokenSource struct {
	s *oidcTokenSource
}

// NewOIDCTokenSource returns a token source for audience that fetches
// with fetch.
func NewOIDCTokenSource(audience string, fetch OIDCFetchFunc) *OIDCTokenSource {
	return &OIDCTokenSource{s: &oidcTokenSource{audience: audience, fetch: fetch, sem: make(chan struct{}, 1), now: time.Now}}
}

// Token returns a cached or freshly fetched token. Errors never carry it.
func (t *OIDCTokenSource) Token(ctx context.Context) (string, error) {
	return t.s.token(ctx, "")
}

// Refresh drops the cached token and fetches a fresh one.
func (t *OIDCTokenSource) Refresh(ctx context.Context) (string, error) {
	select {
	case t.s.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	t.s.value = ""
	<-t.s.sem
	return t.s.token(ctx, "")
}

// maxOIDCTokenFDReply bounds one broker reply.
const maxOIDCTokenFDReply = 16 << 10

// oidcTokenFDTimeout bounds one broker request when ctx has no deadline.
const oidcTokenFDTimeout = 30 * time.Second

// oidcBrokerCodeRe matches a broker error code.
var oidcBrokerCodeRe = regexp.MustCompile(`^[a-z_]{1,32}$`)

// parseOIDCTokenFD returns the broker fd named by raw, and false unless raw
// is a decimal number of at least 3 (anything else counts as unset).
func parseOIDCTokenFD(raw string) (int, bool) {
	fd, err := strconv.Atoi(strings.TrimSpace(raw))
	return fd, err == nil && fd >= 3
}

// oidcTokenFDClient talks to the broker on one fd. Calls are serialized;
// after any I/O error or timeout the connection is not used again, so a
// late reply can never be read as the answer to a later request.
type oidcTokenFDClient struct {
	fd      int
	mu      sync.Mutex
	once    sync.Once
	conn    net.Conn
	r       *bufio.Reader
	openErr error
	broken  bool
}

// fetch returns an OIDCFetchFunc that sends request.
func (c *oidcTokenFDClient) fetch(request string) OIDCFetchFunc {
	return func(ctx context.Context) (*actionsoidc.Assertion, error) {
		return c.do(ctx, request)
	}
}

func (c *oidcTokenFDClient) do(ctx context.Context, request string) (*actionsoidc.Assertion, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.once.Do(func() {
		// Only a Unix stream socket is taken over; anything else is left
		// untouched.
		if c.openErr = checkBrokerFD(c.fd); c.openErr != nil {
			return
		}
		f := os.NewFile(uintptr(c.fd), "jira-oidc-token-fd")
		if f == nil {
			c.openErr = fmt.Errorf("%s is not an open file descriptor", OIDCTokenFDEnvVar)
			return
		}
		c.conn, c.openErr = net.FileConn(f)
		_ = f.Close()
		if c.openErr == nil {
			c.r = bufio.NewReaderSize(c.conn, 4096)
		}
	})
	if c.openErr != nil {
		return nil, fmt.Errorf("opening the Jira OIDC token broker (%s): %w", OIDCTokenFDEnvVar, c.openErr)
	}
	if c.broken {
		return nil, errors.New("the Jira OIDC token broker connection failed earlier and is not reused")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(oidcTokenFDTimeout)
	}
	_ = c.conn.SetDeadline(deadline)
	if _, err := c.conn.Write([]byte(request)); err != nil {
		c.broken = true
		return nil, fmt.Errorf("requesting a token from the Jira OIDC token broker: %w", err)
	}
	line, err := readBrokerLine(c.r)
	if err != nil {
		c.broken = true
		return nil, fmt.Errorf("reading the Jira OIDC token broker's reply: %w", err)
	}
	if code, isErr := strings.CutPrefix(line, "err "); isErr {
		if !oidcBrokerCodeRe.MatchString(code) {
			code = "unknown"
		}
		return nil, fmt.Errorf("the runner's Jira OIDC token broker answered %s", code)
	}
	value, ok := strings.CutPrefix(line, "ok ")
	if !ok || value == "" {
		return nil, errors.New("the Jira OIDC token broker's reply is malformed")
	}
	iat, exp, err := actionsoidc.ParseJWTLifetime(value)
	if err != nil {
		return nil, fmt.Errorf("the Jira OIDC token broker's token: %w", err)
	}
	return &actionsoidc.Assertion{Value: value, IssuedAt: iat, ExpiresAt: exp}, nil
}

// checkBrokerFD reports an error unless fd is an open AF_UNIX
// SOCK_STREAM socket. It does not close or change fd.
func checkBrokerFD(fd int) error {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return fmt.Errorf("%s is not an open file descriptor", OIDCTokenFDEnvVar)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFSOCK {
		return fmt.Errorf("%s is not a socket", OIDCTokenFDEnvVar)
	}
	if typ, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE); err != nil || typ != syscall.SOCK_STREAM {
		return fmt.Errorf("%s is not a stream socket", OIDCTokenFDEnvVar)
	}
	if sa, err := syscall.Getsockname(fd); err != nil {
		return fmt.Errorf("%s is not a Unix socket", OIDCTokenFDEnvVar)
	} else if _, ok := sa.(*syscall.SockaddrUnix); !ok {
		return fmt.Errorf("%s is not a Unix socket", OIDCTokenFDEnvVar)
	}
	return nil
}

// readBrokerLine reads one "\n"-terminated line of at most
// maxOIDCTokenFDReply bytes, without the newline.
func readBrokerLine(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		chunk, err := r.ReadSlice('\n')
		b.Write(chunk)
		if b.Len() > maxOIDCTokenFDReply {
			return "", errors.New("reply too long")
		}
		if err == nil {
			return strings.TrimSuffix(b.String(), "\n"), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return "", err
		}
	}
}

// withOIDCTokenFD makes an oidc client request a token from the broker on
// fd for every Jira request ("token\n"), and a fresh one ("refresh\n")
// for its one retry after a 401.
func withOIDCTokenFD(fd int) Option {
	c := &oidcTokenFDClient{fd: fd}
	return func(lc *LiveClient) {
		lc.oidcFetch = c.fetch("token\n")
		lc.oidcRefetch = c.fetch("refresh\n")
		lc.oidcPerRequest = true
	}
}
