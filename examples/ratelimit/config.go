package ratelimit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Config describes the RLS client. Quota rules and counters belong to the RLS.
type Config struct {
	Cluster       string    `json:"cluster"`
	Authority     string    `json:"authority,omitempty"`
	Domain        string    `json:"domain"`
	TimeoutMillis uint64    `json:"timeout_ms,omitempty"`
	FailureMode   string    `json:"failure_mode,omitempty"`
	Descriptors   [][]Entry `json:"descriptors"`
	// BuildDescriptors replaces declarative descriptors for programmatic hosts.
	// It must be safe across worker threads and must not block or start I/O.
	BuildDescriptors DescriptorBuilder `json:"-"`
}

// Entry obtains a descriptor value from exactly one source. Headers and filter
// state must already be available when this filter's headers callback runs.
type Entry struct {
	Key         string `json:"key"`
	Value       string `json:"value,omitempty"`
	Header      string `json:"header,omitempty"`
	FilterState string `json:"filter_state,omitempty"`
}

// Policy is an immutable, validated snapshot. Construct it with Compile or Decode.
type Policy struct{ config Config }

// Compile validates and copies config so callers cannot mutate a live policy.
func Compile(c Config) (*Policy, error) {
	if strings.TrimSpace(c.Cluster) == "" || strings.TrimSpace(c.Domain) == "" {
		return nil, fmt.Errorf("cluster and domain are required")
	}
	if c.TimeoutMillis == 0 {
		c.TimeoutMillis = 50
	}
	if c.TimeoutMillis > 60000 {
		return nil, fmt.Errorf("timeout_ms must be at most 60000")
	}
	if c.FailureMode == "" {
		c.FailureMode = "closed"
	}
	if c.FailureMode != "closed" && c.FailureMode != "open" {
		return nil, fmt.Errorf("failure_mode must be closed or open")
	}
	if (len(c.Descriptors) == 0) == (c.BuildDescriptors == nil) {
		return nil, fmt.Errorf("provide either descriptors or BuildDescriptors")
	}
	descriptors := make([][]Entry, len(c.Descriptors))
	for i, entries := range c.Descriptors {
		if len(entries) == 0 {
			return nil, fmt.Errorf("descriptor %d is empty", i)
		}
		descriptors[i] = append([]Entry(nil), entries...)
		for j, entry := range entries {
			sources := 0
			for _, value := range []string{entry.Value, entry.Header, entry.FilterState} {
				if value != "" {
					sources++
				}
			}
			if strings.TrimSpace(entry.Key) == "" || sources != 1 {
				return nil, fmt.Errorf("descriptor %d entry %d needs a key and exactly one nonempty value, header or filter_state", i, j)
			}
			if entry.Header != strings.ToLower(entry.Header) || strings.ContainsAny(entry.Header, " \t\r\n") {
				return nil, fmt.Errorf("descriptor %d entry %d header must be lowercase without whitespace", i, j)
			}
		}
	}
	c.Descriptors = descriptors
	return &Policy{config: c}, nil
}

// Decode is a validating decoder for up.PipelineConfig. Invalid updates leave
// its last good snapshot in place. The JSON schema is independent of its source.
func Decode(data []byte) (*Policy, error) {
	var c Config
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON config object")
	}
	return Compile(c)
}
