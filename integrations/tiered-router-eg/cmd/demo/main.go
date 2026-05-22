package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/dio/transit/integrations/tiered-router-eg/internal/demo"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "control":
		fs := newFlagSet("control")
		addr := fs.String("addr", envOr("ADDR", ":8080"), "listen address")
		initial := fs.String("initial-json", os.Getenv("INITIAL_CONFIG_JSON"), "initial tiered config JSON")
		_ = fs.Parse(os.Args[2:])
		err = demo.RunControl(ctx, *addr, []byte(*initial))
	case "upstream":
		fs := newFlagSet("upstream")
		addr := fs.String("addr", envOr("ADDR", ":8080"), "listen address")
		name := fs.String("name", envOr("UPSTREAM_NAME", "upstream"), "upstream name")
		_ = fs.Parse(os.Args[2:])
		err = demo.RunUpstream(ctx, *addr, *name)
	case "l1":
		err = runL1(ctx, os.Stdout, os.Args[2:])
	case "l2":
		err = runL2(ctx, os.Stdout, os.Args[2:])
	case "dump":
		err = runDump(ctx, os.Stdout, os.Args[2:])
	case "request":
		err = runRequest(ctx, os.Stdout, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: tiered-router-demo control|upstream|l1|l2|dump|request [flags]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "demo commands:")
	fmt.Fprintln(os.Stderr, "  l1 routes --control-url http://127.0.0.1:19080")
	fmt.Fprintln(os.Stderr, "  l1 shards add c --target l2-c.transit-dataplane.svc.cluster.local:80 --prefix c")
	fmt.Fprintln(os.Stderr, "  l2 routes b --control-url http://127.0.0.1:19080")
	fmt.Fprintln(os.Stderr, "  l2 models add b qwen-coder --target upstream-d.transit-dataplane.svc.cluster.local:8080 --provider qwen")
	fmt.Fprintln(os.Stderr, "  request gpt-fast --tag a-demo --gateway-url http://127.0.0.1:19081 --host tiered-router.example.com")
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func runL1(ctx context.Context, out io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: tiered-router-demo l1 routes|shards")
	}
	switch args[0] {
	case "routes":
		fs := newFlagSet("l1 routes")
		controlURL := fs.String("control-url", envOr("CONTROL_URL", "http://127.0.0.1:19080"), "control-plane base URL")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		raw, err := demo.Client{}.L1(ctx, *controlURL)
		if err != nil {
			return err
		}
		return writeRaw(out, raw)
	case "shards":
		return runL1Shards(ctx, out, args[1:])
	default:
		return fmt.Errorf("usage: tiered-router-demo l1 routes|shards")
	}
}

func runL1Shards(ctx context.Context, out io.Writer, args []string) error {
	if len(args) == 0 || args[0] != "add" {
		return fmt.Errorf("usage: tiered-router-demo l1 shards add <name> --target <host:port> --prefix <prefix> [--version <value>]")
	}
	if len(args) < 2 {
		return fmt.Errorf("l1 shards add requires exactly one shard name")
	}
	name := args[1]
	fs := newFlagSet("l1 shards add")
	controlURL := fs.String("control-url", envOr("CONTROL_URL", "http://127.0.0.1:19080"), "control-plane base URL")
	target := fs.String("target", "", "L2 shard target host:port")
	prefixes := fs.String("prefix", "", "comma-separated shard prefixes")
	shard := fs.String("shard", name, "visible shard id")
	status := fs.String("status", "active", "shard status")
	version := fs.String("version", "updated", "config version to publish")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected l1 shards add arguments: %v", fs.Args())
	}
	raw, err := demo.Client{}.AddShard(ctx, *controlURL, demo.ShardUpdate{
		Name:     name,
		Target:   *target,
		Prefixes: splitCSV(*prefixes),
		Shard:    *shard,
		Status:   *status,
		Version:  *version,
	})
	if err != nil {
		return err
	}
	return writeRaw(out, raw)
}

func runL2(ctx context.Context, out io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: tiered-router-demo l2 routes|models")
	}
	switch args[0] {
	case "routes":
		if len(args) < 2 {
			return fmt.Errorf("l2 routes requires one shard name")
		}
		shard := args[1]
		fs := newFlagSet("l2 routes")
		controlURL := fs.String("control-url", envOr("CONTROL_URL", "http://127.0.0.1:19080"), "control-plane base URL")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		raw, err := demo.Client{}.L2(ctx, *controlURL, shard)
		if err != nil {
			return err
		}
		return writeRaw(out, raw)
	case "models":
		return runL2Models(ctx, out, args[1:])
	default:
		return fmt.Errorf("usage: tiered-router-demo l2 routes|models")
	}
}

func runL2Models(ctx context.Context, out io.Writer, args []string) error {
	if len(args) == 0 || args[0] != "add" {
		return fmt.Errorf("usage: tiered-router-demo l2 models add <shard> <name> --target <host:port> --provider <provider>")
	}
	if len(args) < 3 {
		return fmt.Errorf("l2 models add requires shard and model name")
	}
	shard := args[1]
	name := args[2]
	fs := newFlagSet("l2 models add")
	controlURL := fs.String("control-url", envOr("CONTROL_URL", "http://127.0.0.1:19080"), "control-plane base URL")
	target := fs.String("target", "", "upstream target host:port")
	provider := fs.String("provider", "", "provider name")
	authHeader := fs.String("auth-header", "", "authorization header to inject upstream")
	profile := fs.String("profile", "", "shard-local profile id")
	byokKeyID := fs.String("byok-key-id", "", "visible BYOK key id")
	version := fs.String("version", "updated", "config version to publish")
	if err := fs.Parse(args[3:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected l2 models add arguments: %v", fs.Args())
	}
	raw, err := demo.Client{}.AddModel(ctx, *controlURL, demo.ModelUpdate{
		Shard:      shard,
		Name:       name,
		Target:     *target,
		Provider:   *provider,
		AuthHeader: *authHeader,
		Profile:    *profile,
		BYOKKeyID:  *byokKeyID,
		Version:    *version,
	})
	if err != nil {
		return err
	}
	return writeRaw(out, raw)
}

func runDump(ctx context.Context, out io.Writer, args []string) error {
	fs := newFlagSet("dump")
	controlURL := fs.String("control-url", envOr("CONTROL_URL", "http://127.0.0.1:19080"), "control-plane base URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw, err := demo.Client{}.Dump(ctx, *controlURL)
	if err != nil {
		return err
	}
	return writeRaw(out, raw)
}

func runRequest(ctx context.Context, out io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("request requires exactly one model name")
	}
	model := args[0]
	fs := newFlagSet("request")
	gatewayURL := fs.String("gateway-url", envOr("GATEWAY_URL", "http://127.0.0.1:19081"), "Gateway base URL")
	host := fs.String("host", envOr("GATEWAY_HOST", "tiered-router.example.com"), "HTTP Host header")
	path := fs.String("path", "/", "request path")
	tag := fs.String("tag", "", "explicit transit tag")
	tenant := fs.String("tenant", "", "tenant id")
	userKey := fs.String("user-key", "", "user key to hash at L1")
	byokKeyID := fs.String("byok-key-id", "", "BYOK key id to hash at L1")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected request arguments: %v", fs.Args())
	}
	raw, err := demo.Client{}.Request(ctx, demo.GatewayRequest{
		GatewayURL: *gatewayURL,
		Host:       *host,
		Model:      model,
		Path:       *path,
		Tag:        *tag,
		Tenant:     *tenant,
		UserKey:    *userKey,
		BYOKKeyID:  *byokKeyID,
	})
	if err != nil {
		return err
	}
	return writeRaw(out, raw)
}

func splitCSV(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func writeRaw(out io.Writer, raw []byte) error {
	if _, err := out.Write(raw); err != nil {
		return err
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		_, err := fmt.Fprintln(out)
		return err
	}
	return nil
}
