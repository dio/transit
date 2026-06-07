package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	secretv1 "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1"
	secretconnect "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1/adminv1connect"
	"github.com/dio/transit/examples/orange/internal/server/secret"
)

// readSecretValue reads the secret material from --value or stdin.
// If value is "-" or empty and stdin is not a terminal, reads from stdin.
func readSecretValue(value string) ([]byte, error) {
	if value != "" && value != "-" {
		return []byte(value), nil
	}
	stat, _ := os.Stdin.Stat()
	isPipe := (stat.Mode() & os.ModeCharDevice) == 0
	if value == "-" || isPipe {
		b, err := io.ReadAll(bufio.NewReader(os.Stdin))
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		return []byte(strings.TrimRight(string(b), "\r\n")), nil
	}
	return nil, fmt.Errorf("--value is required (or pass '-' to read from stdin)")
}

func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "secret",
		Aliases: []string{"sec"},
		Short:   "Manage secrets",
		Long: `Manage secrets at org, project, or workspace level.

Realm format: <level>/<id>/<purpose>
  org/<org-uuid>/api-keys       visible to all egresses under the org
  proj/<proj-uuid>/api-keys     visible to all egresses under the project
  ws/<ws-uuid>/runtime-keys     visible only to egresses in that workspace`,
	}
	cmd.AddCommand(newSecretListCmd())
	cmd.AddCommand(newSecretSetCmd())
	cmd.AddCommand(newSecretGetCmd())
	cmd.AddCommand(newSecretVersionsCmd())
	cmd.AddCommand(newSecretEnableCmd())
	cmd.AddCommand(newSecretDisableCmd())
	cmd.AddCommand(newSecretRetireCmd())
	cmd.AddCommand(newSecretKEKCmd())
	return cmd
}

func newSecretListCmd() *cobra.Command {
	var realm string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List secrets (by realm prefix)",
		Long: `List secrets filtered by a realm prefix.

Examples:
  orange admin secret list --realm=org/<uuid>/       # all org-level secrets
  orange admin secret list --realm=proj/<uuid>/api-keys
  orange admin secret list                           # all secrets (org-admin only)`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.ListSecrets(context.Background(), connect.NewRequest(&secretv1.ListSecretsRequest{
				Realm: realm,
			}))
			if err != nil {
				return err
			}
			return printSecretSummaries(rc.Printer, resp.Msg.GetSecrets()...)
		},
	}
	cmd.Flags().StringVar(&realm, "realm", "", "realm prefix filter (e.g. org/<uuid>/ or proj/<uuid>/api-keys)")
	return cmd
}

func newSecretSetCmd() *cobra.Command {
	var realm, name, value string
	var enable bool
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Create a new secret version",
		Long: `Create a new secret version under the given realm.

Examples:
  orange admin secret set --realm=org/<uuid>/api-keys --name=anthropic --value=sk-ant-...
  orange admin secret set --realm=ws/<uuid>/certs    --name=server-tls --value=- --enable
  echo "sk-ant-..." | orange admin secret set --realm=org/<uuid>/api-keys --name=anthropic`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if realm == "" {
				return fmt.Errorf("--realm is required (e.g. org/<uuid>/api-keys)")
			}
			if _, _, _, err := secret.ParseRealm(realm); err != nil {
				return err
			}
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			material, err := readSecretValue(value)
			if err != nil {
				return err
			}
			if len(material) == 0 {
				return fmt.Errorf("secret material is empty")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.CreateVersion(context.Background(), connect.NewRequest(&secretv1.CreateVersionRequest{
				Realm:    realm,
				SecretId: name,
				Material: material,
				Enable:   enable,
			}))
			if err != nil {
				return err
			}
			return printSecretVersions(rc.Printer, resp.Msg.GetVersion())
		},
	}
	cmd.Flags().StringVar(&realm, "realm", "", "canonical realm: org/<uuid>/<purpose>, proj/<uuid>/<purpose>, ws/<uuid>/<purpose>")
	cmd.Flags().StringVar(&name, "name", "", "secret name within the realm")
	cmd.Flags().StringVar(&value, "value", "", "secret material; use '-' to read from stdin")
	cmd.Flags().BoolVar(&enable, "enable", true, "enable version immediately")
	return cmd
}

func newSecretGetCmd() *cobra.Command {
	var realm, name string
	cmd := &cobra.Command{
		Use:          "get",
		Short:        "Resolve the active secret version (includes plaintext material)",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if realm == "" {
				return fmt.Errorf("--realm is required")
			}
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.ResolveVersion(context.Background(), connect.NewRequest(&secretv1.ResolveVersionRequest{
				Realm:    realm,
				SecretId: name,
			}))
			if err != nil {
				return err
			}
			v := resp.Msg.GetVersion()
			if rc.Printer.Format != FormatJSON && rc.Printer.Format != FormatYAML {
				rc.Printer.OK(string(v.GetMaterial()))
				return nil
			}
			return printSecretVersions(rc.Printer, v)
		},
	}
	cmd.Flags().StringVar(&realm, "realm", "", "canonical realm")
	cmd.Flags().StringVar(&name, "name", "", "secret name")
	return cmd
}

func newSecretVersionsCmd() *cobra.Command {
	var realm, name string
	cmd := &cobra.Command{
		Use:          "versions",
		Short:        "List all versions of a secret",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if realm == "" {
				return fmt.Errorf("--realm is required")
			}
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.ListVersions(context.Background(), connect.NewRequest(&secretv1.ListVersionsRequest{
				Realm:    realm,
				SecretId: name,
			}))
			if err != nil {
				return err
			}
			return printSecretVersions(rc.Printer, resp.Msg.GetVersions()...)
		},
	}
	cmd.Flags().StringVar(&realm, "realm", "", "canonical realm")
	cmd.Flags().StringVar(&name, "name", "", "secret name")
	return cmd
}

func newSecretEnableCmd() *cobra.Command {
	var realm, name, versionID string
	cmd := &cobra.Command{
		Use:          "enable",
		Short:        "Enable a secret version",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if realm == "" {
				return fmt.Errorf("--realm is required")
			}
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if versionID == "" {
				return fmt.Errorf("--version-id is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.EnableVersion(context.Background(), connect.NewRequest(&secretv1.EnableVersionRequest{
				Realm:     realm,
				SecretId:  name,
				VersionId: versionID,
			}))
			if err != nil {
				return err
			}
			return printSecretVersions(rc.Printer, resp.Msg.GetVersion())
		},
	}
	cmd.Flags().StringVar(&realm, "realm", "", "canonical realm")
	cmd.Flags().StringVar(&name, "name", "", "secret name")
	cmd.Flags().StringVar(&versionID, "version-id", "", "version ID to enable")
	return cmd
}

func newSecretDisableCmd() *cobra.Command {
	var realm, name, versionID string
	cmd := &cobra.Command{
		Use:          "disable",
		Short:        "Disable a secret version (reversible)",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if realm == "" {
				return fmt.Errorf("--realm is required")
			}
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if versionID == "" {
				return fmt.Errorf("--version-id is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.DisableVersion(context.Background(), connect.NewRequest(&secretv1.DisableVersionRequest{
				Realm:     realm,
				SecretId:  name,
				VersionId: versionID,
			}))
			if err != nil {
				return err
			}
			return printSecretVersions(rc.Printer, resp.Msg.GetVersion())
		},
	}
	cmd.Flags().StringVar(&realm, "realm", "", "canonical realm")
	cmd.Flags().StringVar(&name, "name", "", "secret name")
	cmd.Flags().StringVar(&versionID, "version-id", "", "version ID to disable")
	return cmd
}

func newSecretRetireCmd() *cobra.Command {
	var realm, name, versionID string
	var shred bool
	cmd := &cobra.Command{
		Use:          "retire",
		Short:        "Retire a secret version (permanent; use --shred to zero material)",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if realm == "" {
				return fmt.Errorf("--realm is required")
			}
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if versionID == "" {
				return fmt.Errorf("--version-id is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.RetireVersion(context.Background(), connect.NewRequest(&secretv1.RetireVersionRequest{
				Realm:     realm,
				SecretId:  name,
				VersionId: versionID,
				Shred:     shred,
			}))
			if err != nil {
				return err
			}
			return printSecretVersions(rc.Printer, resp.Msg.GetVersion())
		},
	}
	cmd.Flags().StringVar(&realm, "realm", "", "canonical realm")
	cmd.Flags().StringVar(&name, "name", "", "secret name")
	cmd.Flags().StringVar(&versionID, "version-id", "", "version ID to retire")
	cmd.Flags().BoolVar(&shred, "shred", false, "zero the material bytes after retiring")
	return cmd
}

func newSecretKEKCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kek",
		Short: "Manage service KEKs for secret encryption",
	}
	cmd.AddCommand(newSecretKEKCreateCmd())
	cmd.AddCommand(newSecretKEKRotateCmd())
	return cmd
}

func newSecretKEKCreateCmd() *cobra.Command {
	var realm string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Provision a service KEK (pool member when --realm is empty; per-boundary otherwise)",
		Long: `Create a SERVICE_KEK.

  Pool member (default, omit --realm):
    orange admin secret kek create

  Per-boundary (one KEK dedicated to this realm):
    orange admin secret kek create --realm=org/<uuid>/api-keys`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.CreateServiceKEK(context.Background(), connect.NewRequest(&secretv1.CreateServiceKEKRequest{
				Realm: realm,
			}))
			if err != nil {
				return err
			}
			switch rc.Printer.Format {
			case FormatJSON, FormatYAML:
				return rc.Printer.Proto(resp.Msg)
			default:
				rc.Printer.Table("KEK-ID\tVERSION", []string{
					fmt.Sprintf("%s\t%d", resp.Msg.GetKekId(), resp.Msg.GetKekVersion()),
				})
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&realm, "realm", "", "canonical realm for per-boundary KEK (empty = pool member)")
	return cmd
}

func newSecretKEKRotateCmd() *cobra.Command {
	var realm string
	cmd := &cobra.Command{
		Use:          "rotate",
		Short:        "Rotate service KEK(s) under the current master KEK",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.RotateServiceKEK(context.Background(), connect.NewRequest(&secretv1.RotateServiceKEKRequest{
				Realm: realm,
			}))
			if err != nil {
				return err
			}
			switch rc.Printer.Format {
			case FormatJSON, FormatYAML:
				return rc.Printer.Proto(resp.Msg)
			default:
				rows := make([]string, len(resp.Msg.GetRotated()))
				for i, r := range resp.Msg.GetRotated() {
					rows[i] = fmt.Sprintf("%s\t%d\t%d\t%d",
						r.GetKekId(), r.GetOldVersion(), r.GetNewVersion(), r.GetMasterKekVersion())
				}
				rc.Printer.Table("KEK-ID\tOLD-VER\tNEW-VER\tMASTER-VER", rows)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&realm, "realm", "", "canonical realm for per-boundary rotation (empty = rotate all pool members)")
	return cmd
}

func printSecretSummaries(p *Printer, summaries ...*secretv1.SecretSummary) error {
	switch p.Format {
	case FormatJSON, FormatYAML:
		msgs := make([]proto.Message, len(summaries))
		for i, s := range summaries {
			msgs[i] = s
		}
		if len(msgs) == 1 {
			return p.Proto(msgs[0])
		}
		return p.ProtoList(msgs)
	default:
		rows := make([]string, len(summaries))
		for i, s := range summaries {
			rows[i] = fmt.Sprintf("%s\t%s", s.GetRealm(), s.GetSecretId())
		}
		p.Table("REALM\tNAME", rows)
		return nil
	}
}

func printSecretVersions(p *Printer, versions ...*secretv1.SecretVersion) error {
	switch p.Format {
	case FormatJSON, FormatYAML:
		msgs := make([]proto.Message, len(versions))
		for i, v := range versions {
			msgs[i] = v
		}
		if len(msgs) == 1 {
			return p.Proto(msgs[0])
		}
		return p.ProtoList(msgs)
	default:
		rows := make([]string, len(versions))
		for i, v := range versions {
			state := strings.TrimPrefix(v.GetState().String(), "VERSION_STATE_")
			sum := v.GetChecksum()
			if len(sum) > 8 {
				sum = sum[:8]
			}
			rows[i] = fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s",
				v.GetVersionId(), v.GetRealm(), v.GetSecretId(), state, sum, age(v.GetCreatedAt()))
		}
		p.Table("VERSION-ID\tREALM\tNAME\tSTATE\tCHECKSUM\tAGE", rows)
		return nil
	}
}
