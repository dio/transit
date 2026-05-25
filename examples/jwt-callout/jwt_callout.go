package jwtcallout

import (
	"encoding/json"
	"strings"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

	"github.com/dio/transit/up"
)

func init() {
	up.Register("jwt-callout", Handler)
}

// Handler validates a JWT by calling an upstream introspection endpoint.
// It reads Authorization: Bearer <token>, calls the token-introspection cluster,
// and sets x-jwt-sub on the forwarded request if the token is active.
func Handler(w *up.Writer, r *up.Request) {
	token, ok := ParseBearer(r.Header("authorization"))
	if !ok {
		w.SendLocalResponse(401, []byte(`{"error":"missing token"}`), [2]string{"content-type", "application/json"})
		return
	}
	body, _ := json.Marshal(map[string]string{"token": token})
	_, err := w.HTTPCallout(up.HTTPCalloutRequest{
		Cluster: "token-introspection",
		Headers: [][2]string{
			{":method", "POST"},
			{":path", "/introspect"},
			{":scheme", "http"},
			{"host", "token-introspection.local"},
			{"content-type", "application/json"},
		},
		Body:          body,
		TimeoutMillis: 300,
	}, func(result up.HTTPCalloutResult, _ [][2]shared.UnsafeEnvoyBuffer, respBody []shared.UnsafeEnvoyBuffer) {
		bodyStr := ""
		if len(respBody) > 0 {
			bodyStr = respBody[0].ToString()
		}
		code, sub, errBody := CheckIntrospection(result, bodyStr)
		if code != 0 {
			w.SendLocalResponse(code, errBody, [2]string{"content-type", "application/json"})
			return
		}
		w.SetRequestHeader("x-jwt-sub", sub)
	})
	if err != nil {
		w.SendLocalResponse(401, []byte(`{"error":"introspection failed"}`), [2]string{"content-type", "application/json"})
	}
}

// ParseBearer extracts the token from an "Authorization: Bearer <token>" value.
// Returns ("", false) if the header is absent or not a Bearer token.
func ParseBearer(header string) (string, bool) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return "", false
	}
	return token, true
}

type introspectionResponse struct {
	Active bool   `json:"active"`
	Sub    string `json:"sub"`
}

// CheckIntrospection evaluates a token-introspection callout response.
// Returns (0, sub, nil) to pass through with the subject injected as x-jwt-sub,
// or (statusCode, "", errorBody) to reject the request.
func CheckIntrospection(result up.HTTPCalloutResult, body string) (int, string, []byte) {
	if result != up.HTTPCalloutSuccess {
		return 401, "", []byte(`{"error":"introspection failed"}`)
	}
	var resp introspectionResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil || !resp.Active {
		return 401, "", []byte(`{"error":"token inactive"}`)
	}
	return 0, resp.Sub, nil
}
