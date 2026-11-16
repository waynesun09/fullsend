// Package urlutil provides URL validation and integrity-hash utilities
// shared by config (agent entry validation) and harness (remote resource
// resolution) packages. It lives at the leaf of the import graph to
// avoid the config → harness circular dependency.
package urlutil

import (
	"net/url"
	"path"
	"strings"
)

// IsURL returns true if s is a valid HTTPS URL suitable for remote resource references.
func IsURL(s string) bool {
	if s == "" {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" {
		return false
	}
	if u.Host == "" || u.User != nil {
		return false
	}
	if u.Hostname() == "" {
		return false
	}
	if strings.Contains(u.Host, "@") {
		return false
	}
	return true
}

// ParseIntegrityHash extracts the SHA256 hash from a URL fragment (#sha256=...).
// Returns the URL without the fragment, the hash value, and whether a valid hash was found.
// The hash is normalized to lowercase; both "sha256=ABC..." and "sha256=abc..." are accepted.
func ParseIntegrityHash(rawURL string) (cleanURL, hash string, hasHash bool) {
	idx := strings.LastIndex(rawURL, "#")
	if idx == -1 {
		return rawURL, "", false
	}
	fragment := rawURL[idx+1:]
	if !strings.HasPrefix(fragment, "sha256=") {
		return rawURL, "", false
	}
	hash = strings.ToLower(strings.TrimPrefix(fragment, "sha256="))
	if len(hash) != 64 {
		return rawURL, "", false
	}
	for _, c := range hash {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return rawURL, "", false
		}
	}
	return rawURL[:idx], hash, true
}

// MatchingAllowedPrefixInList checks if a URL matches any prefix in the given allowlist.
// Returns the matching prefix or "" if none match. Performs case-insensitive
// comparison with percent-decoding and dot-segment cleaning.
func MatchingAllowedPrefixInList(rawURL string, allowlist []string) string {
	lower := strings.ToLower(rawURL)
	if strings.Contains(lower, "%25") {
		return ""
	}
	normalized, ok := NormalizeURLPath(lower)
	if !ok {
		return ""
	}
	for _, prefix := range allowlist {
		normPrefix, prefixOK := NormalizeURLPath(strings.ToLower(prefix))
		if !prefixOK {
			continue
		}
		if strings.HasPrefix(normalized, normPrefix) {
			return prefix
		}
	}
	return ""
}

// NormalizeURLPath parses a URL, percent-decodes and cleans its path, and
// returns the reconstructed URL string. Returns false if parsing fails.
func NormalizeURLPath(rawURL string) (string, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	unescaped, err := url.PathUnescape(parsed.Path)
	if err != nil {
		return "", false
	}
	if strings.ContainsRune(unescaped, '\\') {
		return "", false
	}
	rawPath := parsed.Path
	parsed.Path = path.Clean(unescaped)
	parsed.RawPath = ""
	if parsed.Path == "." {
		parsed.Path = "/"
	} else if strings.HasSuffix(rawPath, "/") && !strings.HasSuffix(parsed.Path, "/") {
		parsed.Path += "/"
	}
	return parsed.String(), true
}

// IsAtlassianHost reports whether host is an Atlassian-operated host a
// forge OIDC token must never be sent to: api.atlassian.com, atlassian.com
// or atlassian.net, or any subdomain of the last two. The comparison is
// case-insensitive and tolerates a trailing dot. host carries no port.
func IsAtlassianHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	for _, apex := range []string{"atlassian.com", "atlassian.net"} {
		if h == apex || strings.HasSuffix(h, "."+apex) {
			return true
		}
	}
	return false
}

// unicodeFullStops are the characters IDNA maps to "." (U+3002, U+FF0E,
// U+FF61), so a host written with one reaches the same server.
var unicodeFullStops = strings.NewReplacer("\u3002", ".", "\uff0e", ".", "\uff61", ".")

// SameHost reports whether two host names are equal, case-insensitively,
// ignoring a trailing dot and treating the Unicode full stops IDNA maps to
// "." as dots. Ports are not part of a host name.
func SameHost(a, b string) bool {
	norm := func(s string) string {
		return strings.TrimSuffix(strings.ToLower(unicodeFullStops.Replace(strings.TrimSpace(s))), ".")
	}
	return norm(a) != "" && norm(a) == norm(b)
}

// IsLDHHost reports whether host is an ASCII DNS name: dot-separated
// labels of 1-63 letters, digits and hyphens, not starting or ending with
// a hyphen, at most 253 characters, with no empty label (so no trailing
// dot). It refuses any non-ASCII host, which IDNA could map onto another
// name, for example "api.atlassian\u3002com".
func IsLDHHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
