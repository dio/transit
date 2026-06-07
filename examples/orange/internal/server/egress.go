package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	egressv1 "github.com/dio/transit/examples/orange/api/orange/egress/admin/v1"
	egressconnect "github.com/dio/transit/examples/orange/api/orange/egress/admin/v1/adminv1connect"
)

func newEgressCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "egress",
		Short: "Manage egress instances",
	}
	cmd.AddCommand(newEgressGetCmd())
	cmd.AddCommand(newEgressGetByWorkspaceCmd())
	cmd.AddCommand(newEgressBundleCmd())
	cmd.AddCommand(newEgressStatusCmd())
	return cmd
}

func newEgressGetCmd() *cobra.Command {
	var egressID string
	cmd := &cobra.Command{
		Use:          "get",
		Short:        "Get an egress by ID",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if egressID == "" {
				egressID = os.Getenv("ORANGE_EGRESS_ID")
			}
			if egressID == "" {
				return fmt.Errorf("--egress-id is required (or set ORANGE_EGRESS_ID)")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := egressconnect.NewEgressAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.GetEgress(context.Background(), connect.NewRequest(&egressv1.GetEgressRequest{EgressId: egressID}))
			if err != nil {
				return err
			}
			return printEgresses(rc.Printer, resp.Msg.GetEgress())
		},
	}
	cmd.Flags().StringVar(&egressID, "egress-id", "", "egress ID (env: ORANGE_EGRESS_ID)")
	return cmd
}

func newEgressGetByWorkspaceCmd() *cobra.Command {
	var workspaceID string
	cmd := &cobra.Command{
		Use:          "get-by-workspace",
		Short:        "Get the egress for a workspace",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if workspaceID == "" {
				workspaceID = os.Getenv("ORANGE_WS_ID")
			}
			if workspaceID == "" {
				return fmt.Errorf("--workspace-id is required (or set ORANGE_WS_ID)")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := egressconnect.NewEgressAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.GetEgressByWorkspace(context.Background(), connect.NewRequest(&egressv1.GetEgressByWorkspaceRequest{WorkspaceId: workspaceID}))
			if err != nil {
				return err
			}
			return printEgresses(rc.Printer, resp.Msg.GetEgress())
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (env: ORANGE_WS_ID)")
	return cmd
}

func newEgressBundleCmd() *cobra.Command {
	var egressID, workspaceID, out string
	cmd := &cobra.Command{
		Use:   "bundle",
		Short: "Download the egress bootstrap bundle",
		Long: `Downloads the egress bootstrap bundle.

By default writes a tar.gz archive named <egress-id>.tar.gz in the current
directory. Use --out to control the destination:

  --out egress.tar.gz   write tar.gz archive
  --out ./mydir/        write loose files into a directory`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if egressID == "" {
				egressID = os.Getenv("ORANGE_EGRESS_ID")
			}
			if workspaceID == "" {
				workspaceID = os.Getenv("ORANGE_WS_ID")
			}
			if egressID == "" && workspaceID == "" {
				return fmt.Errorf("--egress-id or --workspace-id is required (or set ORANGE_EGRESS_ID / ORANGE_WS_ID)")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := egressconnect.NewEgressAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			if egressID == "" {
				wsResp, err := client.GetEgressByWorkspace(context.Background(), connect.NewRequest(&egressv1.GetEgressByWorkspaceRequest{WorkspaceId: workspaceID}))
				if err != nil {
					return err
				}
				egressID = wsResp.Msg.GetEgress().GetEgressId()
			}
			resp, err := client.GetEgressBundle(context.Background(), connect.NewRequest(&egressv1.GetEgressBundleRequest{EgressId: egressID}))
			if err != nil {
				return err
			}
			b := resp.Msg.GetBundle()
			outPath := out
			if outPath == "" {
				outPath = egressID + ".tar.gz"
			}
			return writeBundle(b, outPath, rc.Printer)
		},
	}
	cmd.Flags().StringVar(&egressID, "egress-id", "", "egress ID (env: ORANGE_EGRESS_ID)")
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (env: ORANGE_WS_ID)")
	cmd.Flags().StringVar(&out, "out", "", "output path: <file>.tar.gz or a directory (default: <egress-id>.tar.gz)")
	return cmd
}

func newEgressStatusCmd() *cobra.Command {
	var egressID string
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Show online/offline status of an egress",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if egressID == "" {
				egressID = os.Getenv("ORANGE_EGRESS_ID")
			}
			if egressID == "" {
				return fmt.Errorf("--egress-id is required (or set ORANGE_EGRESS_ID)")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := egressconnect.NewEgressAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.GetEgress(context.Background(), connect.NewRequest(&egressv1.GetEgressRequest{EgressId: egressID}))
			if err != nil {
				return err
			}
			eg := resp.Msg.GetEgress()
			switch rc.Printer.Format {
			case FormatJSON, FormatYAML:
				return rc.Printer.Proto(eg)
			default:
				onlineStr := egressOnlineStatusString(eg.GetOnlineStatus())
				adminStr := egressAdminStatusString(eg.GetAdminStatus())
				lastSeen := "never"
				if eg.GetLastSeenAt() != nil {
					lastSeen = age(eg.GetLastSeenAt())
				}
				rc.Printer.Table("ID\tADMIN-STATUS\tONLINE-STATUS\tLAST-SEEN", []string{
					fmt.Sprintf("%s\t%s\t%s\t%s", eg.GetEgressId(), adminStr, onlineStr, lastSeen),
				})
				return nil
			}
		},
	}
	cmd.Flags().StringVar(&egressID, "egress-id", "", "egress ID (env: ORANGE_EGRESS_ID)")
	return cmd
}

// bundleFiles returns the ordered set of (name, content) pairs for an egress bundle.
func bundleFiles(b *egressv1.EgressBundle) []struct{ name, content string } {
	configYAML := fmt.Sprintf("server_url: %q\negress_id: %q\nworkspace_id: %q\n",
		b.GetServerUrl(), b.GetEgressId(), b.GetWorkspaceId())
	return []struct{ name, content string }{
		{"identity.crt", b.GetIdentityCertPem()},          // who this egress is (X.509 cert)
		{"egress.key", b.GetEgressKeypairPrivateKeyPem()}, // egress→CP: egress signs tokens; CP verifies with paired public key
		{"paseto-1.pub", b.GetPasetoPublicKey_1Pem()},     // client→egress: offline PASETO token validation, slot 1
		{"paseto-2.pub", b.GetPasetoPublicKey_2Pem()},     // client→egress: offline PASETO token validation, slot 2
		{"config.yaml", configYAML},
	}
}

// writeBundle writes the bundle to outPath. If outPath ends in .tar.gz or .tgz
// it produces a gzip-compressed tar archive; otherwise it treats outPath as a
// directory and writes loose files.
func writeBundle(b *egressv1.EgressBundle, outPath string, p *Printer) error {
	files := bundleFiles(b)

	isTar := strings.HasSuffix(outPath, ".tar.gz") || strings.HasSuffix(outPath, ".tgz")
	if isTar {
		if err := writeBundleTarGz(outPath, files); err != nil {
			return err
		}
	} else {
		if err := writeBundleDir(outPath, files); err != nil {
			return err
		}
	}

	if !p.Quiet {
		fmt.Fprintf(p.Out, "%s\n", outPath)
	}
	return nil
}

func writeBundleTarGz(path string, files []struct{ name, content string }) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	defer func() {
		_ = f.Close()
	}()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	for _, entry := range files {
		if entry.content == "" {
			continue
		}
		data := []byte(entry.content)
		if err := tw.WriteHeader(&tar.Header{
			Name: entry.name,
			Mode: 0o600,
			Size: int64(len(data)),
		}); err != nil {
			return fmt.Errorf("tar header %s: %w", entry.name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return fmt.Errorf("tar write %s: %w", entry.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	return gz.Close()
}

func writeBundleDir(dir string, files []struct{ name, content string }) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	for _, entry := range files {
		if entry.content == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, entry.name), []byte(entry.content), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", entry.name, err)
		}
	}
	return nil
}

// printEgresses renders one or more Egress records using the active output format.
func printEgresses(p *Printer, egresses ...*egressv1.Egress) error {
	switch p.Format {
	case FormatJSON, FormatYAML:
		msgs := make([]proto.Message, len(egresses))
		for i, e := range egresses {
			msgs[i] = e
		}
		if len(msgs) == 1 {
			return p.Proto(msgs[0])
		}
		return p.ProtoList(msgs)
	default:
		rows := make([]string, len(egresses))
		for i, e := range egresses {
			rows[i] = fmt.Sprintf("%s\t%s\t%s\t%s\t%s",
				e.GetEgressId(), e.GetWorkspaceId(),
				egressAdminStatusString(e.GetAdminStatus()),
				egressOnlineStatusString(e.GetOnlineStatus()),
				age(e.GetCreatedAt()))
		}
		p.Table("ID\tWORKSPACE-ID\tADMIN-STATUS\tONLINE-STATUS\tAGE", rows)
		return nil
	}
}

func egressAdminStatusString(s egressv1.EgressStatus) string {
	switch s {
	case egressv1.EgressStatus_EGRESS_STATUS_ACTIVE:
		return "active"
	case egressv1.EgressStatus_EGRESS_STATUS_INACTIVE:
		return "inactive"
	default:
		return "unspecified"
	}
}

func egressOnlineStatusString(s egressv1.EgressOnlineStatus) string {
	switch s {
	case egressv1.EgressOnlineStatus_EGRESS_ONLINE_STATUS_ONLINE:
		return "online"
	case egressv1.EgressOnlineStatus_EGRESS_ONLINE_STATUS_OFFLINE:
		return "offline"
	default:
		return "unknown"
	}
}
