package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	HTTP *http.Client
}

type GatewayRequest struct {
	GatewayURL string
	Host       string
	Target     string // value placed in the JSON body as {"target": ...}
	Path       string
}

// Request sends a POST {"target":"<Target>"} body through the Gateway. This
// is the shape body-router-writer expects.
func (c Client) Request(ctx context.Context, req GatewayRequest) ([]byte, error) {
	path := req.Path
	if path == "" {
		path = "/"
	}
	body, err := json.Marshal(map[string]string{"target": req.Target})
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"content-type": "application/json"}
	if req.Host != "" {
		headers["host"] = req.Host
	}
	return c.do(ctx, http.MethodPost, req.GatewayURL, path, body, headers)
}

func (c Client) do(ctx context.Context, method, baseURL, path string, body []byte, headers map[string]string) ([]byte, error) {
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	url := strings.TrimRight(baseURL, "/") + path
	var r io.Reader
	if len(body) > 0 {
		r = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		if strings.EqualFold(k, "host") {
			httpReq.Host = v
			continue
		}
		httpReq.Header.Set(k, v)
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: status %d: %s", method, url, resp.StatusCode, raw)
	}
	return raw, nil
}
