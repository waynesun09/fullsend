package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/fullsend-ai/fullsend/internal/forge/jira"
)

// jiraTimestampFormats are the layouts Jira Cloud has been observed to
// use for created/updated timestamps. Duplicated from
// jirapoll/discover.go's unexported jiraTimestampFormats/
// parseJiraTimestamp rather than shared, for the same reason
// adf.go/jirapoll's ADF walkers stay duplicated for now: consolidating
// onto one shared helper is a reasonable follow-up once the Jira tracker
// integration stabilizes.
var jiraTimestampFormats = []string{
	"2006-01-02T15:04:05.000-0700",
	time.RFC3339,
}

// parseJiraTimestamp parses a Jira timestamp string.
func parseJiraTimestamp(s string) (time.Time, error) {
	for _, format := range jiraTimestampFormats {
		if t, err := time.Parse(format, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable Jira timestamp: %q", s)
}

// stickyPropertyKey is the Jira comment entity property key under which
// sticky-comment markers are stored. Using a namespaced key avoids
// collisions with other Jira apps.
const stickyPropertyKey = "fullsend.sticky-marker"

// statusPropertyKey stores run-status bookkeeping outside the visible Jira
// comment body. Jira renders HTML comments as text, unlike GitHub and GitLab.
const statusPropertyKey = "fullsend.agent-status"

// stickyMarkerProperty is object-shaped because Jira rejects top-level JSON
// strings in the properties array of the create-comment request.
type stickyMarkerProperty struct {
	Marker string `json:"marker"`
}

type statusCommentProperty struct {
	Marker   string `json:"marker"`
	Terminal bool   `json:"terminal"`
}

// jiraClient is the Jira API surface this adapter needs. Implemented by
// jira.LiveClient; faked in tests.
type jiraClient interface {
	AddIssueLabel(ctx context.Context, issueIDOrKey, label string) error
	GetIssue(ctx context.Context, issueIDOrKey string) (*jira.Issue, error)
	CreateIssue(ctx context.Context, in jira.CreateIssueInput) (*jira.CreatedIssue, error)
	ListComments(ctx context.Context, issueIDOrKey string) ([]jira.Comment, error)
	CreateComment(ctx context.Context, issueIDOrKey, body string) (*jira.Comment, error)
	CreateCommentWithProperties(ctx context.Context, issueIDOrKey, body string, properties []jira.CommentProperty) (*jira.Comment, error)
	UpdateComment(ctx context.Context, issueIDOrKey, commentID, body string) error
	UpdateCommentWithProperties(ctx context.Context, issueIDOrKey, commentID, body string, properties []jira.CommentProperty) error
	DeleteComment(ctx context.Context, issueIDOrKey, commentID string) error
	GetMyself(ctx context.Context) (*jira.User, error)
	LinkIssues(ctx context.Context, inwardIssueKey, outwardIssueKey, linkTypeName string) error
}

var _ jiraClient = (*jira.LiveClient)(nil)

// JiraClient adapts a Jira client to tracker.Client. project is a Jira
// project key (e.g. "PROJ"); (project, number) maps to the issue key
// "PROJ-123".
type JiraClient struct {
	jira    jiraClient
	baseURL string // for browse URLs: {baseURL}/browse/{key}

	// propsMu guards props: the comment properties this client last read
	// (ListComments with expand=properties) or wrote, keyed by
	// "issueKey/commentID". An update resends them with its own property,
	// so it keeps every other key whether Jira merges or replaces the
	// properties of an updated comment.
	propsMu sync.Mutex
	props   map[string][]jira.CommentProperty
}

// rememberProperties records the properties known for a comment.
func (c *JiraClient) rememberProperties(key, commentID string, props []jira.CommentProperty) {
	c.propsMu.Lock()
	defer c.propsMu.Unlock()
	if c.props == nil {
		c.props = make(map[string][]jira.CommentProperty)
	}
	c.props[key+"/"+commentID] = append([]jira.CommentProperty(nil), props...)
}

// knownProperties returns the properties recorded for a comment, or nil
// when this client has neither read nor written it.
func (c *JiraClient) knownProperties(key, commentID string) []jira.CommentProperty {
	c.propsMu.Lock()
	defer c.propsMu.Unlock()
	return append([]jira.CommentProperty(nil), c.props[key+"/"+commentID]...)
}

// commentUpdateProperties builds the properties sent with a comment
// update: the properties resendOtherProperties keeps from known, then
// core's own key set to value.
func commentUpdateProperties(known []jira.CommentProperty, key string, value json.RawMessage) []jira.CommentProperty {
	return append(resendOtherProperties(known, key), jira.CommentProperty{Key: key, Value: value})
}

// resendOtherProperties returns the known properties other than core's own
// key, in order, so an update keeps them whether Jira merges or replaces
// the properties of an updated comment. If Jira turns out to merge them
// per key, this step can return nil, and core then sends only its own
// key and never writes a property it does not own.
func resendOtherProperties(known []jira.CommentProperty, ownKey string) []jira.CommentProperty {
	out := make([]jira.CommentProperty, 0, len(known)+1)
	for _, p := range known {
		if p.Key != ownKey {
			out = append(out, p)
		}
	}
	return out
}

// NewJiraClient returns a tracker.Client backed by jc. baseURL is the
// Jira instance's base URL (e.g. "https://acme.atlassian.net"), used to
// build issue browse URLs. Returns an error if baseURL embeds credentials
// (https://user:token@host): those would otherwise propagate into every
// Issue.URL this client returns, which are commonly logged or displayed.
func NewJiraClient(jc jiraClient, baseURL string) (*JiraClient, error) {
	trimmed := strings.TrimRight(baseURL, "/")
	if err := jira.ValidateBaseURL(trimmed); err != nil {
		return nil, err
	}
	return &JiraClient{jira: jc, baseURL: trimmed}, nil
}

// AuthenticatedUser returns the account ID this client is authenticated
// as (GET /myself), so a caller can tell its own comments from anyone
// else's by Comment.Author.AccountID. Display names are not identities.
func (c *JiraClient) AuthenticatedUser(ctx context.Context) (string, error) {
	me, err := c.jira.GetMyself(ctx)
	if err != nil {
		return "", err
	}
	return me.AccountID, nil
}

// issueKey builds a Jira issue key from a project key and issue number,
// e.g. issueKey("PROJ", 123) = "PROJ-123".
func issueKey(project string, number int) string {
	return fmt.Sprintf("%s-%d", project, number)
}

// GetIssue implements Client.
func (c *JiraClient) GetIssue(ctx context.Context, project string, number int) (*Issue, error) {
	key := issueKey(project, number)
	issue, err := c.jira.GetIssue(ctx, key)
	if err != nil {
		return nil, wrapNotFound(err)
	}
	var issueType string
	if issue.Fields.IssueType != nil {
		issueType = issue.Fields.IssueType.Name
	}
	var containerID int64
	if issue.Fields.Project != nil {
		containerID = parseJiraID(issue.Fields.Project.ID)
	}
	canonicalKey := issue.Key
	if canonicalKey == "" {
		canonicalKey = key
	}
	return &Issue{
		ID:           parseJiraID(issue.ID),
		ContainerID:  containerID,
		Number:       number,
		Title:        issue.Fields.Summary,
		Body:         Body(jira.ADFToMarkdown(issue.Fields.Description)),
		URL:          c.baseURL + "/browse/" + canonicalKey,
		Labels:       issue.Fields.Labels,
		IssueType:    issueType,
		CustomFields: issue.Fields.CustomFields,
		Key:          canonicalKey,
	}, nil
}

// CreateIssue implements Client. opts.IssueType is required (Jira rejects
// a create without an issue type); opts.Parent, when set, makes the new
// issue a child (e.g. a sub-task) of that issue key. body is converted to
// ADF by the underlying client. The returned Issue.Number is parsed from
// the new issue key, so it addresses the issue as (project, number) like
// every other JiraClient method.
func (c *JiraClient) CreateIssue(ctx context.Context, project, title string, body Body, opts CreateIssueOptions) (*Issue, error) {
	if opts.IssueType == "" {
		return nil, fmt.Errorf("tracker: Jira requires an issue type to create an issue")
	}
	created, err := c.jira.CreateIssue(ctx, jira.CreateIssueInput{
		ProjectKey:  project,
		IssueType:   opts.IssueType,
		ParentKey:   opts.Parent,
		Summary:     title,
		Description: string(body),
	})
	if err != nil {
		return nil, wrapNotFound(err)
	}
	number, err := issueNumberFromKey(created.Key)
	if err != nil {
		return nil, err
	}
	return &Issue{
		ID:     parseJiraID(created.ID),
		Number: number,
		Title:  title,
		Body:   body,
		URL:    c.baseURL + "/browse/" + created.Key,
		Key:    created.Key,
	}, nil
}

// parseJiraID converts a Jira issue or project ID string to the immutable
// int64 identity. Jira returns positive numeric ID strings; an invalid value
// yields 0, which callers must treat as an unresolved identity rather than
// guessing (FR-002). jirapoll's poller-side equivalent reports its invalid
// ids via the event-skip warning.
func parseJiraID(id string) int64 {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n < 1 {
		log.Printf("WARNING: invalid Jira id %q, using 0", id)
		return 0
	}
	return n
}

// issueNumberFromKey returns the numeric part of a Jira issue key, e.g.
// issueNumberFromKey("PROJ-123") = 123.
func issueNumberFromKey(key string) (int, error) {
	idx := strings.LastIndex(key, "-")
	if idx <= 0 {
		return 0, fmt.Errorf("tracker: unexpected Jira issue key %q", key)
	}
	number, err := strconv.Atoi(key[idx+1:])
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("tracker: unexpected Jira issue key %q", key)
	}
	return number, nil
}

// ListComments implements Client.
func (c *JiraClient) ListComments(ctx context.Context, project string, number int) ([]Comment, error) {
	key := issueKey(project, number)
	comments, err := c.jira.ListComments(ctx, key)
	if err != nil {
		return nil, wrapNotFound(err)
	}
	result := make([]Comment, len(comments))
	for i, comment := range comments {
		result[i] = fromJiraComment(comment)
	}
	return result, nil
}

// ListJiraComments returns the raw jira.Comment objects (including
// entity properties) for use by FindCommentByMarkerProperty. This is
// separate from ListComments because the generic tracker.Comment type
// does not carry Jira-specific property data.
func (c *JiraClient) ListJiraComments(ctx context.Context, project string, number int) ([]jira.Comment, error) {
	key := issueKey(project, number)
	comments, err := c.jira.ListComments(ctx, key)
	if err != nil {
		return nil, wrapNotFound(err)
	}
	return comments, nil
}

// CreateComment implements Client. The returned Comment's Body is
// ADFToMarkdown(comment.Body), the same representation ListComments uses,
// so tracker.Comment.Body has a stable meaning regardless of which method
// produced it.
func (c *JiraClient) CreateComment(ctx context.Context, project string, number int, body Body) (*Comment, error) {
	key := issueKey(project, number)
	comment, err := c.jira.CreateComment(ctx, key, string(body))
	if err != nil {
		return nil, wrapNotFound(err)
	}
	result := fromJiraComment(*comment)
	return &result, nil
}

// CreateStatusComment implements StatusCommentClient by storing run identity
// and terminal state in a Jira comment entity property.
func (c *JiraClient) CreateStatusComment(ctx context.Context, project string, number int, body Body, marker string, terminal bool) (*Comment, error) {
	key := issueKey(project, number)
	// statusCommentProperty contains only a string and bool, so marshaling
	// cannot fail.
	value, _ := json.Marshal(statusCommentProperty{Marker: marker, Terminal: terminal})
	props := []jira.CommentProperty{{Key: statusPropertyKey, Value: value}}
	comment, err := c.jira.CreateCommentWithProperties(ctx, key, string(body), props)
	if err != nil {
		return nil, wrapNotFound(err)
	}
	c.rememberProperties(key, comment.ID, props)
	result := fromJiraComment(*comment)
	return &result, nil
}

// UpdateStatusComment implements StatusCommentClient. The body and the
// status property travel in one update, so the comment never holds a new
// body without its marker. The update resends every other property this
// client read or wrote for the comment (CreateStatusComment,
// FindStatusComment); for a comment it has neither read nor written, only
// the status property is sent.
func (c *JiraClient) UpdateStatusComment(ctx context.Context, project string, number int, commentID string, body Body, marker string, terminal bool) error {
	key := issueKey(project, number)
	// statusCommentProperty contains only a string and bool, so marshaling
	// cannot fail.
	value, _ := json.Marshal(statusCommentProperty{Marker: marker, Terminal: terminal})
	props := commentUpdateProperties(c.knownProperties(key, commentID), statusPropertyKey, value)
	if err := c.jira.UpdateCommentWithProperties(ctx, key, commentID, string(body), props); err != nil {
		return wrapNotFound(err)
	}
	c.rememberProperties(key, commentID, props)
	return nil
}

// FindStatusComment implements StatusCommentClient. Property lookup is the
// primary path; visible-body scanning keeps pre-property comments compatible.
// The body scan only considers comments with no fullsend property whose
// body opens with marker (see legacyMarkdown and hasLegacyMarker).
func (c *JiraClient) FindStatusComment(ctx context.Context, project string, number int, marker string) (*Comment, bool, error) {
	key := issueKey(project, number)
	comments, err := c.jira.ListComments(ctx, key)
	if err != nil {
		return nil, false, wrapNotFound(err)
	}
	for i := range comments {
		for _, prop := range comments[i].Properties {
			if prop.Key != statusPropertyKey {
				continue
			}
			var stored statusCommentProperty
			if json.Unmarshal(prop.Value, &stored) == nil && stored.Marker == marker {
				c.rememberProperties(key, comments[i].ID, comments[i].Properties)
				result := fromJiraComment(comments[i])
				return &result, stored.Terminal, nil
			}
		}
	}
	for i := range comments {
		if body, ok := legacyMarkdown(comments[i]); ok && hasLegacyMarker(body, marker) {
			c.rememberProperties(key, comments[i].ID, comments[i].Properties)
			result := fromJiraComment(comments[i])
			return &result, strings.Contains(body, "fullsend:status:terminal"), nil
		}
	}
	return nil, false, nil
}

// IsStatusComment implements StatusCommentClient. The legacy body check keeps
// timeline analysis correct while older marker-bearing comments are present.
// It only matches a comment with no fullsend property whose body opens with
// a status marker, so agent text that quotes one is not counted.
func (c *JiraClient) IsStatusComment(ctx context.Context, project string, number int, commentID string) (bool, error) {
	key := issueKey(project, number)
	comments, err := c.jira.ListComments(ctx, key)
	if err != nil {
		return false, wrapNotFound(err)
	}
	for i := range comments {
		if comments[i].ID != commentID {
			continue
		}
		for _, prop := range comments[i].Properties {
			if prop.Key == statusPropertyKey {
				return true, nil
			}
		}
		body, ok := legacyMarkdown(comments[i])
		return ok && hasLegacyMarker(body, StatusMarkerOpener), nil
	}
	return false, nil
}

// CreateCommentWithMarker creates a comment with the sticky marker
// stored as a Jira comment entity property instead of embedded in the
// visible ADF body. This keeps implementation bookkeeping out of the
// user-visible conversation on Jira (which lacks HTML comments and
// would render the marker as plain text).
func (c *JiraClient) CreateCommentWithMarker(ctx context.Context, project string, number int, body Body, marker string) (*Comment, error) {
	key := issueKey(project, number)
	markerValue, err := json.Marshal(stickyMarkerProperty{Marker: marker})
	if err != nil {
		return nil, fmt.Errorf("marshal sticky marker: %w", err)
	}
	props := []jira.CommentProperty{
		{Key: stickyPropertyKey, Value: markerValue},
	}
	comment, err := c.jira.CreateCommentWithProperties(ctx, key, string(body), props)
	if err != nil {
		return nil, wrapNotFound(err)
	}
	result := fromJiraComment(*comment)
	return &result, nil
}

// FindCommentByMarkerProperty returns the first comment that carries
// the sticky marker as a comment entity property, or nil if none is
// found. This is the property-based equivalent of the body-scanning
// approach used for GitHub/GitLab.
//
// If no property match is found, a legacy fallback scans comment bodies
// for the marker string (the old behavior before property-based storage).
// It only matches a comment that carries no fullsend property and whose
// body opens with the marker, the layout legacy comments were written in,
// so another comment that quotes the marker is never picked.
// This allows transparent migration: the first update to a legacy
// comment will set the property and strip the marker from the body.
func (c *JiraClient) FindCommentByMarkerProperty(comments []jira.Comment, marker string) *jira.Comment {
	// Primary: match by entity property.
	for i := range comments {
		if hasStickyMarkerProperty(comments[i], marker) {
			return &comments[i]
		}
	}
	// Legacy fallback: match by body text for pre-property comments.
	for i := range comments {
		if md, ok := legacyMarkdown(comments[i]); ok && hasLegacyMarker(md, marker) {
			return &comments[i]
		}
	}
	return nil
}

// hasStickyMarkerProperty reports whether c carries a sticky marker
// property whose value decodes and exactly matches marker. Mere presence
// of the property key, a malformed value, or a different marker is not a
// match.
func hasStickyMarkerProperty(c jira.Comment, marker string) bool {
	for _, prop := range c.Properties {
		if prop.Key != stickyPropertyKey {
			continue
		}
		var stored stickyMarkerProperty
		if json.Unmarshal(prop.Value, &stored) == nil && stored.Marker == marker {
			return true
		}
	}
	return false
}

// fullsendPropertyPrefix is the key prefix shared by every comment entity
// property fullsend writes (stickyPropertyKey, statusPropertyKey).
const fullsendPropertyPrefix = "fullsend."

// legacyMarkdown returns c's visible body as Markdown, with leading
// whitespace trimmed, for the legacy body-embedded marker checks. ok is
// false when c carries any fullsend comment property: such a comment was
// written under the property-based scheme, so a marker in its body is
// agent- or runner-written text, never the comment's own marker.
func legacyMarkdown(c jira.Comment) (md string, ok bool) {
	for _, prop := range c.Properties {
		if strings.HasPrefix(prop.Key, fullsendPropertyPrefix) {
			return "", false
		}
	}
	return strings.TrimLeftFunc(jira.ADFToMarkdown(c.Body), unicode.IsSpace), true
}

// hasLegacyMarker reports whether md (from legacyMarkdown) opens with
// marker, the layout the body-embedded scheme wrote: the marker first,
// the content after it. Matching only that position keeps a marker
// quoted inside agent-written text from identifying the comment.
func hasLegacyMarker(md, marker string) bool {
	return marker != "" && strings.HasPrefix(md, marker)
}

// MigrateAndUpdateComment updates the body of comment, the sticky
// comment FindCommentByMarkerProperty matched, and stores the sticky
// marker as a comment entity property in the same update, so the body and
// the marker are never written apart. A comment found via the legacy
// body-text path is thereby migrated to the property-based scheme.
//
// The update resends comment.Properties (read with expand=properties),
// with the sticky marker set, so every other property key survives
// whether Jira merges or replaces the properties of an updated comment.
//
// The body written to Jira has the marker stripped (unlike
// GitHub/GitLab where the marker is an invisible HTML comment). The
// caller should pass the body without the marker prefix.
func (c *JiraClient) MigrateAndUpdateComment(ctx context.Context, project string, number int, comment jira.Comment, body Body, marker string) error {
	key := issueKey(project, number)
	value, err := json.Marshal(stickyMarkerProperty{Marker: marker})
	if err != nil {
		return fmt.Errorf("marshal sticky marker: %w", err)
	}
	props := commentUpdateProperties(comment.Properties, stickyPropertyKey, value)
	if err := c.jira.UpdateCommentWithProperties(ctx, key, comment.ID, string(body), props); err != nil {
		return wrapNotFound(err)
	}
	return nil
}

// UpdateComment implements Client. Unlike GitHub/GitLab, Jira's
// update-comment endpoint needs the issue key, which is why number is
// part of the signature here.
func (c *JiraClient) UpdateComment(ctx context.Context, project string, number int, commentID string, body Body) error {
	key := issueKey(project, number)
	return wrapNotFound(c.jira.UpdateComment(ctx, key, commentID, string(body)))
}

// DeleteComment implements Client.
func (c *JiraClient) DeleteComment(ctx context.Context, project string, number int, commentID string) error {
	key := issueKey(project, number)
	return wrapNotFound(c.jira.DeleteComment(ctx, key, commentID))
}

// LinkIssues implements Linker. The from issue is sent as Jira's
// inwardIssue and the to issue as its outwardIssue, so Jira records the
// link in the link type's outward direction: for "Blocks", the from issue
// blocks the to issue.
func (c *JiraClient) LinkIssues(ctx context.Context, fromProject string, fromNumber int, toProject string, toNumber int, linkType string) error {
	return wrapNotFound(c.jira.LinkIssues(ctx, issueKey(fromProject, fromNumber), issueKey(toProject, toNumber), linkType))
}

// MoveIssue implements Mover. Jira Cloud has no synchronous per-issue
// move: relocating an issue means submitting an asynchronous bulk-move
// task, polling it to a terminal state, and resolving the new issue key,
// with field mappings to negotiate whenever the destination project's
// issue types, workflow, or mandatory fields differ from the source's.
// None of that is implemented yet, so this reports ErrNotSupported.
func (c *JiraClient) MoveIssue(_ context.Context, sourceProject string, number int, targetProject string) (*MoveResult, error) {
	return nil, fmt.Errorf("move %s to project %s: %w", issueKey(sourceProject, number), targetProject, ErrNotSupported)
}

// fromJiraComment converts a jira.Comment to a tracker.Comment. HTMLURL is
// deliberately left empty: Jira's comment permalink format isn't confirmed
// against real Jira Cloud behavior, so rather than guess at a URL shape
// that might be wrong, it's left unset instead of risking a broken link.
//
// Author is UpdateAuthor when the comment has been edited, not Author:
// someone with Edit-All-Comments can rewrite another user's comment, and
// this is adapted from jirapoll/discover.go's attribute-to-the-editor
// logic (ADR 0054) so edited content isn't misattributed to the original
// author. Note that tracker.Comment.Author is a display name only —
// unlike jirapoll, which authorizes on the stable AccountID, nothing here
// makes Author a trustworthy identifier for authorization decisions; a
// future consumer needing that would have to add one.
func fromJiraComment(c jira.Comment) Comment {
	author := c.Author.DisplayName
	if commentEdited(c) {
		author = c.UpdateAuthor.DisplayName
	}
	return Comment{
		ID:        c.ID,
		Body:      Body(jira.ADFToMarkdown(c.Body)),
		Author:    author,
		CreatedAt: c.Created,
	}
}

// commentEdited reports whether c has genuinely been edited since
// creation. UpdateAuthor.AccountID alone is Jira's signal for this
// (observed Cloud behavior, not a documented contract — see
// jirapoll/discover.go's identical hedge), so this additionally requires
// Updated to parse and be after Created, mirroring jirapoll's own
// edit-detection gate for defense in depth against that signal alone
// being wrong.
func commentEdited(c jira.Comment) bool {
	if c.UpdateAuthor.AccountID == "" {
		return false
	}
	created, err := parseJiraTimestamp(c.Created)
	if err != nil {
		return false
	}
	updated, err := parseJiraTimestamp(c.Updated)
	if err != nil {
		return false
	}
	return updated.After(created)
}

// AddIssueLabel adds a label to a Jira work item, preserving existing labels.
func (c *JiraClient) AddIssueLabel(ctx context.Context, project string, number int, label string) error {
	return c.jira.AddIssueLabel(ctx, issueKey(project, number), label)
}
