package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/dio/transit/integrations/cluster-router-eg/internal/demo"
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
		initial := fs.String("initial-json", os.Getenv("INITIAL_CONFIG_JSON"), "initial route config JSON")
		_ = fs.Parse(os.Args[2:])
		err = demo.RunControl(ctx, *addr, []byte(*initial))
	case "upstream":
		fs := newFlagSet("upstream")
		addr := fs.String("addr", envOr("ADDR", ":8080"), "listen address")
		name := fs.String("name", envOr("UPSTREAM_NAME", "upstream"), "upstream name")
		_ = fs.Parse(os.Args[2:])
		err = demo.RunUpstream(ctx, *addr, *name)
	case "routes":
		err = runRoutes(ctx, os.Stdout, os.Args[2:])
	case "dump":
		err = runDump(ctx, os.Stdout, os.Args[2:])
	case "models":
		err = runModels(ctx, os.Stdout, os.Args[2:])
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
	fmt.Fprintln(os.Stderr, "usage: cluster-router-demo control|upstream|routes|dump|models|request [flags]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "demo commands:")
	fmt.Fprintln(os.Stderr, "  routes --control-url http://127.0.0.1:19080")
	fmt.Fprintln(os.Stderr, "  dump --control-url http://127.0.0.1:19080")
	fmt.Fprintln(os.Stderr, "  models add gpt-slow --target upstream-a.default.svc.cluster.local:8080 --provider openai --auth-header 'Bearer slow-token'")
	fmt.Fprintln(os.Stderr, "  request gpt-slow --gateway-url http://127.0.0.1:19081 --host cluster-router.example.com")
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

func runRoutes(ctx context.Context, out io.Writer, args []string) error {
	fs := newFlagSet("routes")
	controlURL := fs.String("control-url", envOr("CONTROL_URL", "http://127.0.0.1:19080"), "control-plane base URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw, err := demo.Client{}.Routes(ctx, *controlURL)
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

func runModels(ctx context.Context, out io.Writer, args []string) error {
	if len(args) == 0 || args[0] != "add" {
		return fmt.Errorf("usage: cluster-router-demo models add <name> --target <host:port> --provider <provider> [--auth-header <value>] [--version <value>]")
	}
	if len(args) < 2 {
		return fmt.Errorf("models add requires exactly one model name")
	}
	name := args[1]
	fs := newFlagSet("models add")
	controlURL := fs.String("control-url", envOr("CONTROL_URL", "http://127.0.0.1:19080"), "control-plane base URL")
	target := fs.String("target", "", "upstream target host:port")
	provider := fs.String("provider", "", "provider name")
	authHeader := fs.String("auth-header", "", "authorization header to inject upstream")
	version := fs.String("version", "updated", "config version to publish")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected models add arguments: %v", fs.Args())
	}
	raw, err := demo.Client{}.AddModel(ctx, *controlURL, demo.ModelUpdate{
		Name:       name,
		Target:     *target,
		Provider:   *provider,
		AuthHeader: *authHeader,
		Version:    *version,
	})
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
	host := fs.String("host", envOr("GATEWAY_HOST", "cluster-router.example.com"), "HTTP Host header")
	path := fs.String("path", "/", "request path")
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
	})
	if err != nil {
		return err
	}
	return writeRaw(out, raw)
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
