package e2etest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// EmulatorEnvironment is the ENVIRONMENT value that points the behaviour
// suite at a GitHub emulator instead of github.com. In that mode the suite
// takes the forge root from BEHAVIOUR_GITHUB_URL, uses the token in
// GH_TOKEN for every org (no mint e2e role, even inside GitHub Actions)
// and creates its pool org on the emulator when it is missing.
const EmulatorEnvironment = "emulator"

// EmulatorURLEnv names the variable that holds the emulator's root URL,
// e.g. http://127.0.0.1:18302. The REST API is served under /api/v3.
const EmulatorURLEnv = "BEHAVIOUR_GITHUB_URL"

// EmulatorMode reports whether ENVIRONMENT selects the GitHub emulator.
func EmulatorMode() bool {
	return strings.TrimSpace(os.Getenv("ENVIRONMENT")) == EmulatorEnvironment
}

// ConfigureEmulatorForge points this process, and every fullsend CLI it
// starts, at the emulator named by BEHAVIOUR_GITHUB_URL: it sets
// GITHUB_SERVER_URL to the root and GITHUB_API_URL to <root>/api/v3.
//
// The harness sets them itself instead of expecting them in the
// environment because an Actions runner already exports both for
// github.com, and a workflow cannot override GITHUB_* variables.
func ConfigureEmulatorForge() (string, error) {
	root, err := emulatorRoot(os.Getenv(EmulatorURLEnv))
	if err != nil {
		return "", err
	}
	for k, v := range map[string]string{
		"GITHUB_SERVER_URL": root,
		"GITHUB_API_URL":    root + "/api/v3",
	} {
		if err := os.Setenv(k, v); err != nil {
			return "", fmt.Errorf("setting %s: %w", k, err)
		}
	}
	return root, nil
}

func emulatorRoot(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", fmt.Errorf("ENVIRONMENT=%s needs %s (the emulator root, e.g. http://127.0.0.1:18302)", EmulatorEnvironment, EmulatorURLEnv)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%s=%q is not an http(s) URL", EmulatorURLEnv, raw)
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s=%q must be the emulator root, with no path", EmulatorURLEnv, raw)
	}
	return raw, nil
}

// EnsureEmulatorOrg creates org on the emulator through the GitHub
// Enterprise site-admin route (POST /admin/organizations) when it does
// not exist yet. adminLogin becomes the org's first owner. It is a no-op
// for an org that already exists.
func EnsureEmulatorOrg(ctx context.Context, token, org, adminLogin string) error {
	base := githubAPIBaseURL()
	status, body, err := emulatorRequest(ctx, http.MethodGet, base+"/orgs/"+url.PathEscape(org), token, nil)
	if err != nil {
		return fmt.Errorf("looking up org %s: %w", org, err)
	}
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
	default:
		return fmt.Errorf("looking up org %s: unexpected status %d: %s", org, status, body)
	}
	payload, _ := json.Marshal(map[string]string{"login": org, "admin": adminLogin})
	status, body, err = emulatorRequest(ctx, http.MethodPost, base+"/admin/organizations", token, payload)
	if err != nil {
		return fmt.Errorf("creating org %s: %w", org, err)
	}
	if status != http.StatusCreated {
		return fmt.Errorf("creating org %s: unexpected status %d: %s", org, status, body)
	}
	return nil
}

// EmulatorViewerLogin returns the login of the token's user, used as the
// owner of orgs the harness creates.
func EmulatorViewerLogin(ctx context.Context, token string) (string, error) {
	status, body, err := emulatorRequest(ctx, http.MethodGet, githubAPIBaseURL()+"/user", token, nil)
	if err != nil {
		return "", fmt.Errorf("reading the token's user: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("reading the token's user: unexpected status %d: %s", status, body)
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &user); err != nil || user.Login == "" {
		return "", fmt.Errorf("reading the token's user: no login in %s", body)
	}
	return user.Login, nil
}

func emulatorRequest(ctx context.Context, method, target, token string, payload []byte) (int, []byte, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := emulatorHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, body, nil
}

// emulatorHTTPClient talks to the emulator root the harness was given; it is
// a first-party endpoint, so a timeout and a bounded body are enough.
var emulatorHTTPClient = &http.Client{Timeout: 30 * time.Second}
