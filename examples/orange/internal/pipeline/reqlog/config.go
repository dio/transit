package reqlog

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/tidwall/sjson"
)

const defaultMaxBody = 65536

var (
	cfgMu sync.RWMutex
	cfg   = filterConfig{
		RecordRequestHeaders:  true,
		RecordResponseHeaders: true,
		MaxBodyBytes:          defaultMaxBody,
	}
)

type filterConfig struct {
	RecordRequestHeaders  bool              `json:"record_request_headers"`
	RecordResponseHeaders bool              `json:"record_response_headers"`
	RecordRequestBody     bool              `json:"record_request_body"`
	RecordResponseBody    bool              `json:"record_response_body"`
	MaxBodyBytes          int               `json:"max_body_bytes"`
	FieldFilter           FieldFilterConfig `json:"field_filter"`
}

// FieldFilterConfig is the JSON-serialisable config for header and body
// field filtering. Fields are processed in order:
//
//  1. AllowRequestHeaders / AllowResponseHeaders: if non-empty, only headers
//     whose lowercase name appears in the list are kept.
//  2. DenyRequestHeaders / DenyResponseHeaders: headers whose lowercase name
//     appears in the list are dropped (applied after the allow pass).
//  3. BodyRemovePaths: sjson dot-notation paths deleted from both request and
//     response bodies (validated JSON only; skipped on parse error).
//  4. BodyRedactPaths: sjson dot-notation paths whose string values are
//     replaced with "[REDACTED]".
type FieldFilterConfig struct {
	DenyRequestHeaders   []string `json:"deny_request_headers"`
	DenyResponseHeaders  []string `json:"deny_response_headers"`
	AllowRequestHeaders  []string `json:"allow_request_headers"`
	AllowResponseHeaders []string `json:"allow_response_headers"`
	BodyRedactPaths      []string `json:"body_redact_paths"`
	BodyRemovePaths      []string `json:"body_remove_paths"`
}

// FieldFilter is the compiled, ready-to-apply form of FieldFilterConfig.
type FieldFilter struct {
	denyReqHdrs   map[string]struct{}
	denyRespHdrs  map[string]struct{}
	allowReqHdrs  map[string]struct{}
	allowRespHdrs map[string]struct{}
	bodyRedact    []string
	bodyRemove    []string
}

// NewFieldFilter compiles a FieldFilterConfig into a FieldFilter.
func NewFieldFilter(c FieldFilterConfig) *FieldFilter {
	return &FieldFilter{
		denyReqHdrs:   toLowerSet(c.DenyRequestHeaders),
		denyRespHdrs:  toLowerSet(c.DenyResponseHeaders),
		allowReqHdrs:  toLowerSet(c.AllowRequestHeaders),
		allowRespHdrs: toLowerSet(c.AllowResponseHeaders),
		bodyRedact:    c.BodyRedactPaths,
		bodyRemove:    c.BodyRemovePaths,
	}
}

// Apply mutates r in place, removing or redacting fields per the filter rules.
func (f *FieldFilter) Apply(r *Record) {
	r.RequestHeaders = applyHeaderFilter(r.RequestHeaders, f.allowReqHdrs, f.denyReqHdrs)
	r.ResponseHeaders = applyHeaderFilter(r.ResponseHeaders, f.allowRespHdrs, f.denyRespHdrs)
	if len(f.bodyRemove)+len(f.bodyRedact) > 0 {
		r.RequestBody = applyBodyFilter(r.RequestBody, f.bodyRedact, f.bodyRemove)
		r.ResponseBody = applyBodyFilter(r.ResponseBody, f.bodyRedact, f.bodyRemove)
	}
}

// applyHeaderFilter returns the subset of hdrs that pass the allow/deny rules.
// allow is checked first; deny is applied to whatever remains.
func applyHeaderFilter(hdrs [][2]string, allow, deny map[string]struct{}) [][2]string {
	if len(allow) == 0 && len(deny) == 0 {
		return hdrs
	}
	out := hdrs[:0:len(hdrs)]
	for _, h := range hdrs {
		name := strings.ToLower(h[0])
		if len(allow) > 0 {
			if _, ok := allow[name]; !ok {
				continue
			}
		}
		if _, ok := deny[name]; ok {
			continue
		}
		out = append(out, h)
	}
	return out
}

// applyBodyFilter applies remove-then-redact transforms to a JSON body string.
// Non-JSON bodies and empty strings are returned unchanged.
func applyBodyFilter(body string, redact, remove []string) string {
	if body == "" {
		return body
	}
	if !json.Valid([]byte(body)) {
		return body
	}
	var err error
	for _, path := range remove {
		if body, err = sjson.Delete(body, path); err != nil {
			return body
		}
	}
	for _, path := range redact {
		if body, err = sjson.Set(body, path, "[REDACTED]"); err != nil {
			return body
		}
	}
	return body
}

func toLowerSet(ss []string) map[string]struct{} {
	if len(ss) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		m[strings.ToLower(s)] = struct{}{}
	}
	return m
}
