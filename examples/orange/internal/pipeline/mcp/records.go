package mcp

import "time"

type record struct {
	Method            string
	Route             string
	SelectedBackend   string
	Backends          []string
	Tool              string
	LegCount          int
	FailedLegs        int
	Outcome           string
	ErrorClass        string
	Duration          time.Duration
	PublicSessionHash string
	RequestID         string
	Start             time.Time
}

func (r record) finish(duration time.Duration) record {
	r.Duration = duration
	if r.Outcome == "" {
		r.Outcome = "unknown"
	}
	return r
}
