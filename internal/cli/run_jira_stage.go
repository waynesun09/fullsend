package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/fullsend-ai/fullsend/internal/config"
	"github.com/fullsend-ai/fullsend/internal/forge/jira"
	"github.com/fullsend-ai/fullsend/internal/harness"
	"github.com/fullsend-ai/fullsend/internal/sticky"
	"github.com/fullsend-ai/fullsend/internal/tracker"
	"github.com/fullsend-ai/fullsend/internal/ui"
)

// Jira stage comment posted from core. A post-script whose harness opts in
// with `FULLSEND_JIRA_STAGE_RESULT: "${FULLSEND_JIRA_STAGE_RESULT}"` in
// env.runner reports the pull or merge request it opened or pushed to by
// writing one line of JSON to the file that variable names:
//
//	{"verb":"opened","url":"https://github.com/o/r/pull/7"}
//
// After the post-script exits, core reads and checks the result and posts
// the stage comment on the run's Jira work item, with the Jira client it
// built before any script ran and the forge URL of the repository it
// captured at start. Nothing is re-read from config.yaml at that point.
// The comment is posted whenever the file holds a valid result, also after
// a failed post-script. Without the opt-in nothing changes.

// jiraStageResultEnv names the result file in the post-script.
const jiraStageResultEnv = "FULLSEND_JIRA_STAGE_RESULT"

// jiraStageMarker is the sticky marker of the code stage comment.
const jiraStageMarker = "<!-- fullsend:code-agent -->"

// jiraStageResultMaxBytes bounds the result file.
const jiraStageResultMaxBytes = 2048

// jiraStageResultOptIn is the exact env.runner value that opts in.
const jiraStageResultOptIn = "${" + jiraStageResultEnv + "}"

// jiraStage is what core captured at start for the stage comment.
type jiraStage struct {
	client  *tracker.JiraClient
	project string
	number  int
	forge   string // "github" or "gitlab"
	// hostPort is the forge's lower-case host, with ":port" unless the
	// port is 443; prefix is the repository's URL path, for example
	// "/acme/widgets" or "/gitlab/grp/proj".
	hostPort    string
	prefix      string
	keepHistory bool
	// path is the result file, created at start in a fresh 0700
	// directory.
	path string
}

// jiraStageOptedIn reports whether h declares exactly
// FULLSEND_JIRA_STAGE_RESULT: "${FULLSEND_JIRA_STAGE_RESULT}" in
// env.runner (or runner_env). Another key with that value is no opt-in.
func jiraStageOptedIn(h *harness.Harness) bool {
	if strings.TrimSpace(h.RunnerEnv[jiraStageResultEnv]) == jiraStageResultOptIn {
		return true
	}
	return h.Env != nil && strings.TrimSpace(h.Env.Runner[jiraStageResultEnv]) == jiraStageResultOptIn
}

// jiraStageNewClientFn builds the Jira tracker client. Override in tests.
var jiraStageNewClientFn = func(tracking *config.TrackingConfig) (*tracker.JiraClient, error) {
	tc, err := newJiraTrackerClientFromConfig(tracking)
	if err != nil {
		return nil, err
	}
	jc, ok := tc.(*tracker.JiraClient)
	if !ok {
		return nil, errors.New("the Jira tracker client has an unexpected type")
	}
	return jc, nil
}

// jiraStageRepoURL returns the run's repository URL captured at start:
// CI_PROJECT_URL on GitLab, GITHUB_SERVER_URL (default
// https://github.com) plus GITHUB_REPOSITORY on GitHub. It must be https
// with an ASCII host and no user info, query or fragment.
func jiraStageRepoURL(forgePlatform string, getenv func(string) string) (hostPort, prefix string, err error) {
	raw := getenv("CI_PROJECT_URL")
	if forgePlatform != "gitlab" {
		server := strings.TrimRight(getenv("GITHUB_SERVER_URL"), "/")
		if server == "" {
			server = "https://github.com"
		}
		repo := getenv("GITHUB_REPOSITORY")
		if repo == "" {
			return "", "", errors.New("GITHUB_REPOSITORY is not set")
		}
		raw = server + "/" + repo
	}
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Path == "" {
		return "", "", errors.New("the run's repository URL is not a plain https URL")
	}
	hostPort, ok := jiraStageHostPort(u)
	if !ok {
		return "", "", errors.New("the run's repository URL has no ASCII host name")
	}
	return hostPort, u.Path, nil
}

// stageLDHHostRe matches an ASCII DNS name: dot-separated labels of 1-63
// letters, digits and hyphens, not starting or ending with a hyphen, and
// no empty label. Any other host (for example one with a Unicode
// character that folds to ASCII) is refused.
var stageLDHHostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// jiraStageHostPort returns u's lower-case host, with ":port" unless the
// port is empty or 443, and false unless the host is an ASCII DNS name and
// the port, when present, is non-empty and numeric.
func jiraStageHostPort(u *url.URL) (string, bool) {
	host, port := u.Hostname(), u.Port()
	if !stageLDHHostRe.MatchString(host) || len(host) > 253 || strings.HasSuffix(u.Host, ":") {
		return "", false
	}
	if port != "" {
		if _, err := strconv.Atoi(port); err != nil {
			return "", false
		}
	}
	hostPort := strings.ToLower(host)
	if port != "" && port != "443" {
		hostPort += ":" + port
	}
	return hostPort, true
}

// armJiraStage captures, before any script runs, what the stage comment
// needs, and creates the empty result file. It returns nil when the
// harness did not opt in or the run is not Jira-sourced. When the harness
// opted in on a Jira-sourced run, anything missing (the repository URL,
// the Jira client, the result file) is an error that fails the run at
// start. keepHistory is called only after the opt-in check. The cleanup
// func removes the result directory. Any inherited
// FULLSEND_JIRA_STAGE_RESULT is cleared, so the opt-in reference expands
// empty and is dropped everywhere but the post-script launch.
func armJiraStage(h *harness.Harness, sOpts statusOpts, tracking *config.TrackingConfig, forgePlatform string, keepHistory func() bool, getenv func(string) string, roots []string) (*jiraStage, func(), error) {
	noop := func() {}
	_ = os.Unsetenv(jiraStageResultEnv)
	if !jiraStageOptedIn(h) || sOpts.trackerSource != "jira" {
		return nil, noop, nil
	}
	if sOpts.trackerProject == "" || sOpts.statusNum <= 0 {
		return nil, noop, errors.New("Jira stage comment: the run has no Jira work item")
	}
	st := &jiraStage{project: sOpts.trackerProject, number: sOpts.statusNum, forge: "github"}
	if forgePlatform == "gitlab" {
		st.forge = "gitlab"
	}
	var err error
	if st.hostPort, st.prefix, err = jiraStageRepoURL(forgePlatform, getenv); err != nil {
		return nil, noop, fmt.Errorf("Jira stage comment: %w", err)
	}
	if st.client, err = jiraStageNewClientFn(tracking); err != nil {
		return nil, noop, fmt.Errorf("Jira stage comment: building the Jira client: %w", err)
	}
	st.keepHistory = keepHistory()
	cleanup, err := st.createResultFile(roots)
	if err != nil {
		return nil, noop, fmt.Errorf("Jira stage comment: creating the result file: %w", err)
	}
	return st, cleanup, nil
}

// jiraStageResultBaseFn returns the directory the result directory is
// created in. Override in tests.
var jiraStageResultBaseFn = func() string {
	if d := os.Getenv("RUNNER_TEMP"); d != "" && filepath.IsAbs(d) {
		return d
	}
	return os.TempDir()
}

// createResultFile creates a fresh 0700 directory outside roots (the run
// directory, workspace and host_files sources) with an empty 0600 result
// file in it, and returns a func that removes the directory.
func (st *jiraStage) createResultFile(roots []string) (func(), error) {
	base := ""
	for _, b := range []string{jiraStageResultBaseFn(), os.TempDir()} {
		if b != "" && !pathUnderAny(b, roots) {
			base = b
			break
		}
	}
	if base == "" {
		return nil, errors.New("no temporary directory outside the workspace, run directory and host_files sources")
	}
	dir, err := os.MkdirTemp(base, "fullsend-jira-stage-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := os.Chmod(dir, 0o700); err != nil {
		cleanup()
		return nil, err
	}
	st.path = filepath.Join(dir, "result.json")
	f, err := os.OpenFile(st.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		cleanup()
		return nil, err
	}
	_ = f.Close()
	return cleanup, nil
}

// pathUnderAny reports whether path is one of roots or inside one, after
// resolving symlinks in the longest existing prefix of each.
func pathUnderAny(path string, roots []string) bool {
	resolve := func(p string) string {
		abs, err := filepath.Abs(p)
		if err != nil {
			return p
		}
		rest := ""
		for dir := abs; ; dir = filepath.Dir(dir) {
			if r, err := filepath.EvalSymlinks(dir); err == nil {
				return filepath.Join(r, rest)
			}
			if filepath.Dir(dir) == dir {
				return abs
			}
			rest = filepath.Join(filepath.Base(dir), rest)
		}
	}
	p := resolve(path)
	for _, r := range roots {
		if r == "" {
			continue
		}
		rel, err := filepath.Rel(resolve(r), p)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// jiraStageResultRoots are the roots the result directory stays out of.
func jiraStageResultRoots(h *harness.Harness, runDir string) []string {
	roots := []string{runDir, os.Getenv("GITHUB_WORKSPACE"), os.Getenv("CI_PROJECT_DIR"), os.Getenv("FULLSEND_DIR")}
	for _, hf := range h.HostFiles {
		// Both the raw source and its expansion (the path bootstrapEnv
		// copies), so a source such as ${RUNNER_TEMP} is covered too.
		roots = append(roots, hf.Src, safeExpandEnv(hf.Src))
	}
	return roots
}

// attach gives cmd, the post-script, the result file and returns the func
// to call once it has exited: it reads, checks and posts the result. A
// nil stage attaches nothing and posts nothing.
func (st *jiraStage) attach(cmd *exec.Cmd, printer *ui.Printer) func(context.Context) error {
	if st == nil {
		return func(context.Context) error { return nil }
	}
	cmd.Env = append(cmd.Env, jiraStageResultEnv+"="+st.path)
	return func(ctx context.Context) error {
		return st.postJiraStageComment(ctx, st.path, printer)
	}
}

// jiraStageResult is the post-script's result.
type jiraStageResult struct {
	Verb string
	URL  string
}

// readJiraStageResult reads the result file untrusted: no symlink, a
// regular file, at most jiraStageResultMaxBytes, then parseJiraStageResult.
// ok is false for an empty file.
func readJiraStageResult(path string) (res jiraStageResult, ok bool, err error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return res, false, fmt.Errorf("opening the stage result: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return res, false, fmt.Errorf("reading the stage result: %w", err)
	}
	if !info.Mode().IsRegular() {
		return res, false, errors.New("the stage result is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, jiraStageResultMaxBytes+1))
	if err != nil {
		return res, false, fmt.Errorf("reading the stage result: %w", err)
	}
	if len(data) > jiraStageResultMaxBytes {
		return res, false, fmt.Errorf("the stage result exceeds %d bytes", jiraStageResultMaxBytes)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return res, false, nil
	}
	res, err = parseJiraStageResult(data)
	return res, err == nil, err
}

// parseJiraStageResult parses one JSON object whose keys are exactly
// "verb" and "url", each once and each a string, with nothing after it.
// Keys match byte for byte: no case folding.
func parseJiraStageResult(data []byte) (jiraStageResult, error) {
	var res jiraStageResult
	bad := errors.New(`the stage result is not one JSON object with exactly the string keys "verb" and "url"`)
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return res, bad
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		key, isString := tok.(string)
		if err != nil || !isString || (key != "verb" && key != "url") || seen[key] {
			return res, bad
		}
		seen[key] = true
		tok, err = dec.Token()
		value, isString := tok.(string)
		if err != nil || !isString {
			return res, bad
		}
		if key == "verb" {
			res.Verb = value
		} else {
			res.URL = value
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return res, bad
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return res, errors.New("the stage result has data after its JSON object")
	}
	if !seen["verb"] || !seen["url"] {
		return res, bad
	}
	return res, nil
}

// stageExcerptRe keeps a bounded, printable excerpt for messages.
var stageExcerptRe = regexp.MustCompile(`[^\x20-\x7e]`)

func stageExcerpt(s string) string {
	s = stageExcerptRe.ReplaceAllString(s, "?")
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}

// githubPRPathRe and gitlabMRPathRe match the PR or MR part of a path.
var (
	githubPRPathRe = regexp.MustCompile(`^/pull/[1-9][0-9]*$`)
	gitlabMRPathRe = regexp.MustCompile(`^/-/merge_requests/[1-9][0-9]*$`)
)

// checkJiraStageResult validates res against what core captured at start
// and returns the normalised URL to post: a known verb, and a plain https
// URL (no user info, query, fragment, "#" or empty port) on exactly the
// run's forge host and port whose path is a PR (GitHub) or MR (GitLab) of
// the run's own repository.
func (st *jiraStage) checkJiraStageResult(res jiraStageResult) (string, error) {
	if res.Verb != "opened" && res.Verb != "pushed" {
		return "", fmt.Errorf("the stage result's verb %q is not opened or pushed", stageExcerpt(res.Verb))
	}
	notPlain := fmt.Errorf("the stage result's url %q is not a plain https URL", stageExcerpt(res.URL))
	if strings.Contains(res.URL, "#") {
		return "", notPlain
	}
	u, err := url.Parse(res.URL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" {
		return "", notPlain
	}
	hostPort, ok := jiraStageHostPort(u)
	if !ok || hostPort != st.hostPort {
		return "", fmt.Errorf("the stage result's url host %q is not the run's forge host %s", stageExcerpt(u.Host), st.hostPort)
	}
	rest, ok := strings.CutPrefix(u.Path, st.prefix)
	re := githubPRPathRe
	if st.forge == "gitlab" {
		re = gitlabMRPathRe
	}
	if !ok || !re.MatchString(rest) {
		return "", fmt.Errorf("the stage result's url %q is not a pull or merge request of %s%s", stageExcerpt(res.URL), st.hostPort, st.prefix)
	}
	return (&url.URL{Scheme: "https", Host: hostPort, Path: u.Path}).String(), nil
}

// jiraStageCommentBody returns the stage comment text for the normalised
// URL.
func (st *jiraStage) jiraStageCommentBody(verb, normalisedURL string) string {
	noun := "pull request"
	if st.forge == "gitlab" {
		noun = "merge request"
	}
	action := "opened"
	if verb == "pushed" {
		action = "pushed new commits to"
	}
	key := st.project + "-" + strconv.Itoa(st.number)
	return fmt.Sprintf("**Code stage:** %s %s %s for %s.\n\nReview and merge happen on the %s.", action, noun, normalisedURL, key, noun)
}

// postJiraStageComment reads, checks and posts the stage result at path.
// An empty result posts nothing. An invalid result or a failed post is an
// error, which fails the step.
func (st *jiraStage) postJiraStageComment(ctx context.Context, path string, printer *ui.Printer) error {
	res, ok, err := readJiraStageResult(path)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	normalised, err := st.checkJiraStageResult(res)
	if err != nil {
		return err
	}
	cfg := sticky.Config{Marker: jiraStageMarker, KeepHistory: st.keepHistory, MaxSize: jira.MaxMarkdownBytes}
	if _, err := postJiraStickyComment(ctx, st.client, st.project, st.number, st.jiraStageCommentBody(res.Verb, normalised), cfg, false, printer); err != nil {
		return fmt.Errorf("posting the Jira stage comment: %w", err)
	}
	return nil
}
