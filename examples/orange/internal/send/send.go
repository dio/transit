// Package send provides helpers for sending JSON error responses via [up.Writer].
package send

import (
	"encoding/json"
	"fmt"

	"github.com/dio/transit/up"
)

type errorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

// Error type constants matching OpenAI's error taxonomy.
const (
	InvalidRequestError = "invalid_request_error"
	AuthenticationError = "authentication_error"
	PermissionError     = "permission_error"
	NotFoundError       = "not_found_error"
	RateLimitError      = "rate_limit_error"
	InternalServerError = "internal_server_error"
	ServiceUnavailable  = "service_unavailable_error"
)

var contentTypeJSON = [2]string{"content-type", "application/json"}

// Error sends a JSON local response shaped after the OpenAI error envelope:
//
//	{"error":{"message":"…","type":"…","param":null,"code":"…"}}
//
// content-type: application/json is always prepended; callers may append extra
// headers via the variadic argument.
func Error(w *up.Writer, status int, errType, code, msg string, headers ...[2]string) {
	body, _ := json.Marshal(errorBody{Error: errorDetail{
		Message: msg,
		Type:    errType,
		Code:    code,
	}})
	all := make([][2]string, 0, 1+len(headers))
	all = append(all, contentTypeJSON)
	all = append(all, headers...)
	w.SendLocalResponse(status, body, all...)
}

// Errorf is like [Error] but formats the message via [fmt.Sprintf].
func Errorf(w *up.Writer, status int, errType, code, format string, args ...any) {
	Error(w, status, errType, code, fmt.Sprintf(format, args...))
}
