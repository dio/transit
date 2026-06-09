package server

// egress_serve_local_img.go — "img" top-level REPL command for image generation.
//
// Commands (dispatched by cmdImg):
//
//	img <prompt>         POST /v1/images/generations → PNG saved to imgdir
//	img models           GET  /v1/models (image-capable models only)
//	img set model <name> pin image model (empty/auto = gpt-image-1)
//	img set dir   <path> output directory for generated images (default: /tmp)
//	img status           show current settings
//	img help             this help

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const imgDefault = "gpt-image-1"

// hasImageTag reports whether a model's metadata.tags slice contains "images".
func hasImageTag(item map[string]any) bool {
	meta, ok := item["metadata"].(map[string]any)
	if !ok {
		return false
	}
	tags, ok := meta["tags"].([]any)
	if !ok {
		return false
	}
	for _, t := range tags {
		if s, ok := t.(string); ok && s == "images" {
			return true
		}
	}
	return false
}

// cmdImg routes "img ..." subcommands.
func (s *serveLocalState) cmdImg(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return s.imgStatus()
	}
	switch args[0] {
	case "models":
		return s.imgDoModels(ctx)

	case "set":
		return s.imgSet(args[1:])

	case "status":
		return s.imgStatus()

	case "help", "?":
		printImgHelp()
		return nil

	default:
		prompt := strings.Join(args, " ")
		return s.imgDoGenerate(ctx, prompt)
	}
}

// ── generate ──────────────────────────────────────────────────────────────────

func (s *serveLocalState) imgDoGenerate(ctx context.Context, prompt string) error {
	model := s.imgModel
	if model == "" {
		model = imgDefault
	}
	payload := map[string]any{
		"model":  model,
		"prompt": prompt,
		"n":      1,
	}
	fmt.Fprintf(os.Stderr, "→ POST %s/v1/images/generations  key=%s model=%s\n",
		s.llmBaseURL, s.llmKey, model)
	resp, elapsed, err := s.llmRequest(ctx, "POST", "/v1/images/generations", payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "← HTTP %d  %.0fms\n", resp.StatusCode, float64(elapsed.Milliseconds()))
	result, err := decodeJSONBody(resp.Body, resp.Status)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return llmPrintError(result)
	}

	data, ok := result["data"].([]any)
	if !ok || len(data) == 0 {
		return fmt.Errorf("no image data in response")
	}
	item, ok := data[0].(map[string]any)
	if !ok {
		return fmt.Errorf("unexpected image item format")
	}

	b64Str, hasB64 := item["b64_json"].(string)
	urlStr, hasURL := item["url"].(string)
	var imgBytes []byte
	switch {
	case hasB64 && b64Str != "":
		imgBytes, err = base64.StdEncoding.DecodeString(b64Str)
		if err != nil {
			return fmt.Errorf("decode image: %w", err)
		}
	case hasURL && urlStr != "":
		imgBytes, err = fetchURL(ctx, urlStr)
		if err != nil {
			return fmt.Errorf("fetch image url: %w", err)
		}
	default:
		return fmt.Errorf("no image data (b64_json or url) in response")
	}

	if err := os.MkdirAll(s.imgDir, 0o755); err != nil {
		return fmt.Errorf("create imgdir: %w", err)
	}
	outPath := filepath.Join(s.imgDir, fmt.Sprintf("orange-img-%d.png", time.Now().UnixMilli()))
	if err := os.WriteFile(outPath, imgBytes, 0o644); err != nil {
		return fmt.Errorf("save image: %w", err)
	}
	fmt.Println(outPath)

	if revised, ok := item["revised_prompt"].(string); ok && revised != "" && revised != prompt {
		fmt.Fprintf(os.Stderr, "  revised prompt: %s\n", revised)
	}
	return nil
}

// ── models ────────────────────────────────────────────────────────────────────

func (s *serveLocalState) imgDoModels(ctx context.Context) error {
	fmt.Fprintf(os.Stderr, "→ GET %s/v1/models  key=%s\n", s.llmBaseURL, s.llmKey)
	resp, elapsed, err := s.llmRequest(ctx, "GET", "/v1/models", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "← HTTP %d  %.0fms\n", resp.StatusCode, float64(elapsed.Milliseconds()))
	result, err := decodeJSONBody(resp.Body, resp.Status)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return llmPrintError(result)
	}

	data, ok := result["data"].([]any)
	if !ok {
		llmPrintJSON(result)
		return nil
	}

	type entry struct{ id, provider string }
	var entries []entry
	for _, raw := range data {
		m, ok := raw.(map[string]any)
		if !ok || !hasImageTag(m) {
			continue
		}
		id, _ := m["id"].(string)
		provider, _ := m["owned_by"].(string)
		if id != "" {
			entries = append(entries, entry{id, provider})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })

	for _, e := range entries {
		if e.provider != "" {
			fmt.Printf("  %-40s  %s\n", e.id, e.provider)
		} else {
			fmt.Println(" ", e.id)
		}
	}
	if len(entries) == 0 {
		fmt.Fprintf(os.Stderr, "  no models tagged \"images\" found\n")
	} else {
		fmt.Fprintf(os.Stderr, "  %d image model(s)\n", len(entries))
	}
	return nil
}

// ── set / status ──────────────────────────────────────────────────────────────

func (s *serveLocalState) imgSet(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: img set model|dir <value>")
	}
	switch args[0] {
	case "model":
		if args[1] == "-" || args[1] == "auto" {
			s.imgModel = ""
			fmt.Println("model → auto (gpt-image-1)")
		} else {
			s.imgModel = args[1]
			fmt.Printf("model → %s\n", s.imgModel)
		}
	case "dir":
		s.imgDir = expandTilde(args[1])
		fmt.Printf("dir → %s\n", s.imgDir)
	default:
		return fmt.Errorf("unknown setting %q — try: model, dir", args[0])
	}
	return nil
}

func (s *serveLocalState) imgStatus() error {
	model := s.imgModel
	if model == "" {
		model = "auto (" + imgDefault + ")"
	}
	fmt.Printf("base:  %s\n", s.llmBaseURL)
	fmt.Printf("key:   %s\n", s.llmKey)
	fmt.Printf("model: %s\n", model)
	fmt.Printf("dir:   %s\n", s.imgDir)
	return nil
}

// expandTilde replaces a leading "~" with the user's home directory so paths
// entered in the REPL work the same as in the shell.
func expandTilde(path string) string {
	if path == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
	}
	if strings.HasPrefix(path, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, path[2:])
		}
	}
	return path
}

// fetchURL downloads the content at url and returns the raw bytes.
func fetchURL(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// ── help ──────────────────────────────────────────────────────────────────────

func printImgHelp() {
	fmt.Print(`
img — image generation commands (routes through local Envoy on port 8080)

Generate:
  img <prompt>               POST /v1/images/generations → PNG saved to dir
  img models                 list image-capable models (dall-e-*, gpt-image-*, ...)

Config:
  img set model <name>       pin image model; 'auto' or '-' to reset (default: gpt-image-1)
  img set dir   <path>       output directory for generated images (default: /tmp)
  img status                 show current settings

Examples:
  img a cat riding a bicycle at sunset
  img set dir ~/Desktop
  img set model gpt-image-1
  img a futuristic city skyline
  img models
  img set model auto

Tip: the saved file path is printed to stdout; metadata (URL, key, HTTP status,
     timing, revised prompt) goes to stderr.

`)
}
