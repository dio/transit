// Package metadata demonstrates metadata-driven routing: an HTTP filter reads
// static route filter_metadata, writes the tier to dynamic metadata for access
// logs, and passes the tier via filter state so the Cluster Extension can pick
// the correct upstream host.
package metadata

import "github.com/dio/transit/up"

func init() {
	up.Register("metadata-router", handle)
}

const (
	metaNS  = "example.routing"
	metaKey = "tier"
	fsKey   = "meta.tier"
)

func handle(w *up.Writer, _ *up.Request) {
	tier := ""
	if buf, ok := w.GetMetadataString(up.MetadataSourceRoute, metaNS, metaKey); ok {
		tier = buf.String()
	}

	if tier == "" {
		tier = "standard"
	}

	// Write to filter state so the Cluster Extension can read it.
	w.SetFilterState(fsKey, tier)

	// Write to dynamic metadata so it appears in access logs.
	w.SetMetadata(metaNS, metaKey, tier)

	w.Log(up.LogInfo, "metadata-router: tier=%s", tier)
}
