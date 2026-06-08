package rls

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"

	rlsconfig "github.com/envoyproxy/ratelimit/src/config"
)

// HTTPLoader returns a Loader that GETs url on each poll and parses the
// response body as a single YAML rate limit config. The URL basename is used
// as the config name. Useful for development or when orange CP exposes a plain
// HTTP config endpoint.
func HTTPLoader(url string, client *http.Client) Loader {
	if client == nil {
		client = http.DefaultClient
	}
	name := filepath.Base(url)
	return func(ctx context.Context) (rlsconfig.RateLimitConfig, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		return parseYAML(name, string(b))
	}
}

// parseYAML parses a single YAML blob into a RateLimitConfig.
// ConfigFileContentToYaml and NewRateLimitConfigImpl panic on invalid input;
// both are recovered and returned as errors.
func parseYAML(name, content string) (cfg rlsconfig.RateLimitConfig, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("rls: parse %q: %v", name, r)
		}
	}()
	root := rlsconfig.ConfigFileContentToYaml(name, content)
	cfg = rlsconfig.NewRateLimitConfigImpl(
		[]rlsconfig.RateLimitConfigToLoad{{Name: name, ConfigYaml: root}},
		newNoopManager(),
		true,
	)
	return cfg, nil
}
