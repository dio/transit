// Package scopes defines canonical API key scope strings and helpers that
// implement the resource:action[context] format described in the
// scope-binding design doc.
//
// Format: <resource>:<action>[context]
//   - resource  — e.g. "secret", "token", "org", "user"
//   - action    — e.g. "read", "write", "issue", "admin", "*"
//   - [context] — optional workspace ID (only for workspace-scoped actions)
//
// Matching rule: a scope "token:issue[ws-abc]" in a key satisfies a required
// scope of "token:issue" (base match). The literal "admin" satisfies any
// requirement (superadmin bypass, kept for proto annotation compatibility).
package scopes

import "strings"

// Base scope constants (no context). These match proto annotation strings
// exactly and must not be renamed without regenerating protos.
const (
	// OrgAdmin is the super-admin scope. A key carrying this scope satisfies
	// any scope check. The short form "admin" is kept as an alias for proto
	// annotation compatibility.
	OrgAdmin = "org:admin"
	Admin    = "admin" // proto annotation alias — do not remove

	// UserRead is the minimal scope issued to every new user key.
	UserRead = "user:read"

	// Base forms of workspace-scoped actions. Keys carry the contextual form
	// (e.g. "token:issue[ws-abc]"); the base form appears in proto annotations.
	SecretRead           = "secret:read"
	SecretWrite          = "secret:write"
	TokenIssue           = "token:issue"
	EgressBundleDownload = "egress-bundle:download"
)

// FormatScoped returns a contextual scope string: "base[context]".
func FormatScoped(base, context string) string {
	return base + "[" + context + "]"
}

// ParseScope splits a scope string into its base and optional context parts.
// "token:issue[ws-abc]" → base="token:issue", context="ws-abc".
// "user:read"           → base="user:read",   context="".
func ParseScope(s string) (base, context string) {
	b, c, _ := strings.Cut(s, "[")
	c = strings.TrimSuffix(c, "]")
	return b, c
}

// WorkspaceMemberScopes returns the standard set of scopes appended to every
// user key when the user is added to wsID.
func WorkspaceMemberScopes(wsID string) []string {
	return []string{
		FormatScoped(SecretRead, wsID),
		FormatScoped(SecretWrite, wsID),
		FormatScoped(TokenIssue, wsID),
	}
}

// HasScope reports whether the slice of scopes satisfies required.
//
// Match rules (first match wins):
//  1. Any scope equal to Admin ("admin") or OrgAdmin ("org:admin") → true
//  2. Exact match: scope == required
//  3. Base match:  base(scope) == required  (e.g. "token:issue[ws]" satisfies "token:issue")
func HasScope(scopes []string, required string) bool {
	for _, sc := range scopes {
		if sc == Admin || sc == OrgAdmin {
			return true
		}
		if sc == required {
			return true
		}
		base, _ := ParseScope(sc)
		if base == required {
			return true
		}
	}
	return false
}

// RemoveWorkspaceScopes filters out all scopes whose context equals wsID.
func RemoveWorkspaceScopes(scopes []string, wsID string) []string {
	out := scopes[:0:0]
	for _, sc := range scopes {
		_, ctx := ParseScope(sc)
		if ctx != wsID {
			out = append(out, sc)
		}
	}
	return out
}

// AppendWorkspaceScopes adds workspace member scopes for wsID, deduplicating
// against existing entries.
func AppendWorkspaceScopes(existing []string, wsID string) []string {
	have := make(map[string]bool, len(existing))
	for _, s := range existing {
		have[s] = true
	}
	out := make([]string, len(existing))
	copy(out, existing)
	for _, s := range WorkspaceMemberScopes(wsID) {
		if !have[s] {
			out = append(out, s)
		}
	}
	return out
}
