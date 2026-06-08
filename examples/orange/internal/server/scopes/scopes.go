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

	// RLPolicyWrite grants the ability to author rate-limit policy entries.
	// Keys carry the contextual form: "rl-policy:write[ws-id]" for workspace-
	// level authority, or "rl-policy:write[ws-id/user]" for user-level authority.
	// A workspace-level grant also covers all user-level scopes within that workspace.
	// Tier management (create/update/delete) always requires admin.
	RLPolicyWrite = "rl-policy:write"
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

// WorkspaceMemberScopes returns the standard set of scopes appended to a key
// when the user is added to wsID. When userID is non-empty the user's own
// rl-policy:write[wsID/userID] scope is included so they can author
// rate-limit policies for their own scope without needing admin.
func WorkspaceMemberScopes(wsID, userID string) []string {
	ss := []string{
		FormatScoped(SecretRead, wsID),
		FormatScoped(SecretWrite, wsID),
		FormatScoped(TokenIssue, wsID),
	}
	if userID != "" {
		ss = append(ss, FormatScoped(RLPolicyWrite, wsID+"/"+userID))
	}
	return ss
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

// AuthorizedForRLScope reports whether callerScopes authorizes writing the
// given scopeKey (a 1- or 2-segment rate-limit policy scope key, e.g.
// "ws-abc" or "ws-abc/adi").
//
// Authorization rules:
//
//	admin / org:admin                      → any scopeKey
//	rl-policy:write[X]  where X == scopeKey → exact scope
//	rl-policy:write[X]  where scopeKey starts with X+"/"
//	                                       → workspace grant covers user scopes
func AuthorizedForRLScope(callerScopes []string, scopeKey string) bool {
	for _, sc := range callerScopes {
		if sc == Admin || sc == OrgAdmin {
			return true
		}
		base, ctx := ParseScope(sc)
		if base != RLPolicyWrite || ctx == "" {
			continue
		}
		if ctx == scopeKey || strings.HasPrefix(scopeKey, ctx+"/") {
			return true
		}
	}
	return false
}

// WorkspaceAccessForRLPolicy reports whether callerScopes grants any
// rl-policy:write access within workspaceID — either a workspace-level grant
// ("rl-policy:write[wsID]") or any user-level grant within it
// ("rl-policy:write[wsID/user]"). Used to gate tier-catalog reads so that
// policy authors can discover available tiers without needing admin.
func WorkspaceAccessForRLPolicy(callerScopes []string, workspaceID string) bool {
	for _, sc := range callerScopes {
		if sc == Admin || sc == OrgAdmin {
			return true
		}
		base, ctx := ParseScope(sc)
		if base != RLPolicyWrite || ctx == "" {
			continue
		}
		if ctx == workspaceID || strings.HasPrefix(ctx, workspaceID+"/") {
			return true
		}
	}
	return false
}

// RLScopeContexts returns all scope-key contexts the caller is explicitly
// authorized for (i.e. the [X] values from rl-policy:write[X] tokens).
// Returns nil when the caller has admin/org:admin authority (no context restriction).
func RLScopeContexts(callerScopes []string) (contexts []string, isAdmin bool) {
	for _, sc := range callerScopes {
		if sc == Admin || sc == OrgAdmin {
			return nil, true
		}
	}
	for _, sc := range callerScopes {
		base, ctx := ParseScope(sc)
		if base == RLPolicyWrite && ctx != "" {
			contexts = append(contexts, ctx)
		}
	}
	return contexts, false
}

// RemoveWorkspaceScopes filters out all scopes whose context equals wsID or
// starts with wsID+"/" (covers both workspace-level and user-level sub-scopes,
// e.g. "rl-policy:write[ws1/adi]" is removed when wsID=="ws1").
func RemoveWorkspaceScopes(scopes []string, wsID string) []string {
	out := scopes[:0:0]
	for _, sc := range scopes {
		_, ctx := ParseScope(sc)
		if ctx != wsID && !strings.HasPrefix(ctx, wsID+"/") {
			out = append(out, sc)
		}
	}
	return out
}

// appendScopes is the shared dedup logic for Append* helpers.
func appendScopes(existing, add []string) []string {
	have := make(map[string]bool, len(existing))
	for _, s := range existing {
		have[s] = true
	}
	out := make([]string, len(existing))
	copy(out, existing)
	for _, s := range add {
		if !have[s] {
			out = append(out, s)
			have[s] = true
		}
	}
	return out
}

// AppendWorkspaceScopes adds workspace member scopes for wsID (without a
// user-level rl-policy scope), deduplicating against existing entries.
// Use when the key is not bound to a specific named user.
func AppendWorkspaceScopes(existing []string, wsID string) []string {
	return appendScopes(existing, WorkspaceMemberScopes(wsID, ""))
}

// AppendWorkspaceScopesForUser adds the full workspace member scope set for
// wsID including the user's own rl-policy:write[wsID/userID] grant,
// deduplicating against existing entries.
func AppendWorkspaceScopesForUser(existing []string, wsID, userID string) []string {
	return appendScopes(existing, WorkspaceMemberScopes(wsID, userID))
}
