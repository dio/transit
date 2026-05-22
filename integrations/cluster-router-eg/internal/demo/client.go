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
	Model      string
	Path       string
}

func (c Client) Routes(ctx context.Context, controlURL string) ([]byte, error) {
	return c.get(ctx, controlURL, "/routes.json")
}

func (c Client) Dump(ctx context.Context, controlURL string) ([]byte, error) {
	return c.get(ctx, controlURL, "/dump")
}

func (c Client) AddModel(ctx context.Context, controlURL string, update ModelUpdate) ([]byte, error) {
	raw, err := json.Marshal(update)
	if err != nil {
		return nil, err
	}
	return c.doJSON(ctx, http.MethodPost, controlURL, "/models", raw, nil)
}

func (c Client) Request(ctx context.Context, req GatewayRequest) ([]byte, error) {
	path := req.Path
	if path == "" {
		path = "/"
	}
	headers := map[string]string{"x-model": req.Model}
	if req.Host != "" {
		headers["host"] = req.Host
	}
	return c.doJSON(ctx, http.MethodGet, req.GatewayURL, path, nil, headers)
}

func (c Client) get(ctx context.Context, baseURL, path string) ([]byte, error) {
	return c.doJSON(ctx, http.MethodGet, baseURL, path, nil, nil)
}

func (c Client) doJSON(ctx context.Context, method, baseURL, path string, body []byte, headers map[string]string) ([]byte, error) {
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	url := strings.TrimRight(baseURL, "/") + path
	var r io.Reader
	if len(body) > 0 {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return nil, err
	}
	if len(body) > 0 {
		req.Header.Set("content-type", "application/json")
	}
	for k, v := range headers {
		if strings.EqualFold(k, "host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
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
