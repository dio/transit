package secret

import (
	"fmt"
	"strings"
)

const (
	LevelOrg  = "org"
	LevelProj = "proj"
	LevelWS   = "ws"
)

// ParseRealm splits "org/<uuid>/api-keys" into (level="org", id="<uuid>", purpose="api-keys").
// Returns an error if the prefix is unknown or fewer than three slash-separated segments exist.
func ParseRealm(realm string) (level, id, purpose string, err error) {
	parts := strings.SplitN(realm, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("realm %q: want <level>/<id>/<purpose> (e.g. org/<uuid>/api-keys)", realm)
	}
	switch parts[0] {
	case LevelOrg, LevelProj, LevelWS:
	default:
		return "", "", "", fmt.Errorf("realm %q: unknown level %q; valid: org, proj, ws", realm, parts[0])
	}
	return parts[0], parts[1], parts[2], nil
}

// AncestryPrefixes returns the realm prefixes that an egress in wsID (under projID and orgID)
// is permitted to resolve. A secret whose realm has any of these prefixes is accessible.
func AncestryPrefixes(wsID, projID, orgID string) []string {
	return []string{
		LevelWS + "/" + wsID + "/",
		LevelProj + "/" + projID + "/",
		LevelOrg + "/" + orgID + "/",
	}
}

// RealmInAncestry reports whether an egress authenticated for wsID/projID/orgID is allowed
// to resolve a secret stored under realm.
func RealmInAncestry(realm, wsID, projID, orgID string) bool {
	for _, pfx := range AncestryPrefixes(wsID, projID, orgID) {
		if strings.HasPrefix(realm, pfx) {
			return true
		}
	}
	return false
}
