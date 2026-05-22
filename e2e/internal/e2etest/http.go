package e2etest

import "net/http"

// CloseBody closes resp.Body when resp is non-nil.
func CloseBody(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}
