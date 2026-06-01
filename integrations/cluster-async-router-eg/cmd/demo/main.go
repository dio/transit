// cluster-async-router-demo is a small CLI + upstream binary used by the
// cluster-async-router-eg integration. It has two modes:
//
//   - "upstream"  — runs the JSON-echo HTTP server inside k8s as upstream-a / -b.
//   - "request"   — host-side CLI: POSTs {"target":"<name>"} through the Gateway.
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

	"github.com/dio/transit/integrations/cluster-async-router-eg/internal/demo"
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
	case "upstream":
		fs := newFlagSet("upstream")
		addr := fs.String("addr", envOr("ADDR", ":8080"), "listen address")
		name := fs.String("name", envOr("UPSTREAM_NAME", "upstream"), "upstream name")
		_ = fs.Parse(os.Args[2:])
		err = demo.RunUpstream(ctx, *addr, *name)
	case "upstream-tls":
		fs := newFlagSet("upstream-tls")
		addr := fs.String("addr", envOr("ADDR", ":8443"), "HTTPS listen address")
		healthz := fs.String("healthz-addr", envOr("HEALTHZ_ADDR", ":8080"), "plaintext /healthz listen address")
		name := fs.String("name", envOr("UPSTREAM_NAME", "upstream"), "upstream name")
		sni := fs.String("expected-sni", envOr("EXPECTED_SNI", ""), "required ClientHello SNI; mismatch fails handshake")
		certFile := fs.String("cert-file", envOr("CERT_FILE", "/etc/tls/tls.crt"), "leaf cert path")
		keyFile := fs.String("key-file", envOr("KEY_FILE", "/etc/tls/tls.key"), "leaf key path")
		_ = fs.Parse(os.Args[2:])
		if *sni == "" {
			err = fmt.Errorf("--expected-sni is required")
		} else {
			err = demo.RunUpstreamTLS(ctx, *addr, *healthz, *name, *sni, *certFile, *keyFile)
		}
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
	fmt.Fprintln(os.Stderr, "usage: cluster-async-router-demo upstream|upstream-tls|request [flags]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  upstream --name upstream-a --addr :8080")
	fmt.Fprintln(os.Stderr, "  upstream-tls --name upstream-c --expected-sni host-c.test --cert-file /etc/tls/tls.crt --key-file /etc/tls/tls.key --addr :8443 --healthz-addr :8080")
	fmt.Fprintln(os.Stderr, "  request --target a --gateway-url http://127.0.0.1:19081 --host cluster-async-router.example.com")
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func runRequest(ctx context.Context, out io.Writer, args []string) error {
	fs := newFlagSet("request")
	gatewayURL := fs.String("gateway-url", envOr("GATEWAY_URL", "http://127.0.0.1:19081"), "Gateway base URL")
	host := fs.String("host", envOr("GATEWAY_HOST", "cluster-async-router.example.com"), "HTTP Host header")
	target := fs.String("target", "", "value placed in {\"target\": ...} body")
	path := fs.String("path", "/", "request path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *target == "" {
		return fmt.Errorf("--target is required")
	}
	raw, err := demo.Client{}.Request(ctx, demo.GatewayRequest{
		GatewayURL: *gatewayURL,
		Host:       *host,
		Target:     *target,
		Path:       *path,
	})
	if err != nil {
		return err
	}
	if _, err := out.Write(raw); err != nil {
		return err
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		_, err = fmt.Fprintln(out)
	}
	return err
}
