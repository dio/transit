package orange

import (
	"context"
	"fmt"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	secretv1 "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1"
	secretconnect "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1/adminv1connect"
)

// parseSecretName splits "realm/secret-id" into (realm, secretID).
// If no slash is present, realm defaults to "default".
func parseSecretName(name string) (realm, secretID string) {
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return name[:i], name[i+1:]
	}
	return "default", name
}

func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "secret",
		Aliases: []string{"sec"},
		Short:   "Manage secrets in a workspace",
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
	var workspaceID, realm string
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List secrets in a workspace",
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
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.ListSecrets(context.Background(), connect.NewRequest(&secretv1.ListSecretsRequest{
				WorkspaceId: workspaceID,
				Realm:       realm,
			}))
			if err != nil {
				return err
			}
			return printSecretSummaries(rc.Printer, resp.Msg.GetSecrets()...)
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (env: ORANGE_WS_ID)")
	cmd.Flags().StringVar(&realm, "realm", "", "filter by realm (omit for all realms)")
	return cmd
}

func newSecretSetCmd() *cobra.Command {
	var workspaceID, name, value string
	var enable bool
	cmd := &cobra.Command{
		Use:          "set",
		Short:        "Create a new secret version (--name realm/secret-id)",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if workspaceID == "" {
				workspaceID = os.Getenv("ORANGE_WS_ID")
			}
			if workspaceID == "" {
				return fmt.Errorf("--workspace-id is required (or set ORANGE_WS_ID)")
			}
			if name == "" {
				return fmt.Errorf("--name is required (format: realm/secret-id or just secret-id for realm=default)")
			}
			if value == "" {
				return fmt.Errorf("--value is required")
			}
			realm, secretID := parseSecretName(name)
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.CreateVersion(context.Background(), connect.NewRequest(&secretv1.CreateVersionRequest{
				WorkspaceId: workspaceID,
				Realm:       realm,
				SecretId:    secretID,
				Material:    []byte(value),
				Enable:      enable,
			}))
			if err != nil {
				return err
			}
			return printSecretVersions(rc.Printer, resp.Msg.GetVersion())
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (env: ORANGE_WS_ID)")
	cmd.Flags().StringVar(&name, "name", "", "secret name as realm/secret-id (e.g. prod/db-password); omit realm for default")
	cmd.Flags().StringVar(&value, "value", "", "secret material (plaintext)")
	cmd.Flags().BoolVar(&enable, "enable", true, "enable version immediately")
	return cmd
}

func newSecretGetCmd() *cobra.Command {
	var workspaceID, name string
	cmd := &cobra.Command{
		Use:          "get",
		Short:        "Resolve the active version of a secret (includes plaintext material)",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if workspaceID == "" {
				workspaceID = os.Getenv("ORANGE_WS_ID")
			}
			if workspaceID == "" {
				return fmt.Errorf("--workspace-id is required (or set ORANGE_WS_ID)")
			}
			if name == "" {
				return fmt.Errorf("--name is required (format: realm/secret-id or just secret-id)")
			}
			realm, secretID := parseSecretName(name)
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.ResolveVersion(context.Background(), connect.NewRequest(&secretv1.ResolveVersionRequest{
				WorkspaceId: workspaceID,
				Realm:       realm,
				SecretId:    secretID,
			}))
			if err != nil {
				return err
			}
			v := resp.Msg.GetVersion()
			// In table mode print just the material; structured formats include all fields.
			if rc.Printer.Format != FormatJSON && rc.Printer.Format != FormatYAML {
				rc.Printer.OK(string(v.GetMaterial()))
				return nil
			}
			return printSecretVersions(rc.Printer, v)
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (env: ORANGE_WS_ID)")
	cmd.Flags().StringVar(&name, "name", "", "secret name as realm/secret-id (e.g. prod/db-password)")
	return cmd
}

func newSecretVersionsCmd() *cobra.Command {
	var workspaceID, name string
	cmd := &cobra.Command{
		Use:          "versions",
		Short:        "List all versions of a secret",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if workspaceID == "" {
				workspaceID = os.Getenv("ORANGE_WS_ID")
			}
			if workspaceID == "" {
				return fmt.Errorf("--workspace-id is required (or set ORANGE_WS_ID)")
			}
			if name == "" {
				return fmt.Errorf("--name is required (format: realm/secret-id or just secret-id)")
			}
			realm, secretID := parseSecretName(name)
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.ListVersions(context.Background(), connect.NewRequest(&secretv1.ListVersionsRequest{
				WorkspaceId: workspaceID,
				Realm:       realm,
				SecretId:    secretID,
			}))
			if err != nil {
				return err
			}
			return printSecretVersions(rc.Printer, resp.Msg.GetVersions()...)
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (env: ORANGE_WS_ID)")
	cmd.Flags().StringVar(&name, "name", "", "secret name as realm/secret-id (e.g. prod/db-password)")
	return cmd
}

func newSecretEnableCmd() *cobra.Command {
	var workspaceID, name, versionID string
	cmd := &cobra.Command{
		Use:          "enable",
		Short:        "Enable a secret version",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if workspaceID == "" {
				workspaceID = os.Getenv("ORANGE_WS_ID")
			}
			if workspaceID == "" {
				return fmt.Errorf("--workspace-id is required (or set ORANGE_WS_ID)")
			}
			if name == "" {
				return fmt.Errorf("--name is required (format: realm/secret-id or just secret-id)")
			}
			if versionID == "" {
				return fmt.Errorf("--version-id is required")
			}
			realm, secretID := parseSecretName(name)
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.EnableVersion(context.Background(), connect.NewRequest(&secretv1.EnableVersionRequest{
				WorkspaceId: workspaceID,
				Realm:       realm,
				SecretId:    secretID,
				VersionId:   versionID,
			}))
			if err != nil {
				return err
			}
			return printSecretVersions(rc.Printer, resp.Msg.GetVersion())
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (env: ORANGE_WS_ID)")
	cmd.Flags().StringVar(&name, "name", "", "secret name as realm/secret-id (e.g. prod/db-password)")
	cmd.Flags().StringVar(&versionID, "version-id", "", "version ID to enable")
	return cmd
}

func newSecretDisableCmd() *cobra.Command {
	var workspaceID, name, versionID string
	cmd := &cobra.Command{
		Use:          "disable",
		Short:        "Disable a secret version (reversible)",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if workspaceID == "" {
				workspaceID = os.Getenv("ORANGE_WS_ID")
			}
			if workspaceID == "" {
				return fmt.Errorf("--workspace-id is required (or set ORANGE_WS_ID)")
			}
			if name == "" {
				return fmt.Errorf("--name is required (format: realm/secret-id or just secret-id)")
			}
			if versionID == "" {
				return fmt.Errorf("--version-id is required")
			}
			realm, secretID := parseSecretName(name)
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.DisableVersion(context.Background(), connect.NewRequest(&secretv1.DisableVersionRequest{
				WorkspaceId: workspaceID,
				Realm:       realm,
				SecretId:    secretID,
				VersionId:   versionID,
			}))
			if err != nil {
				return err
			}
			return printSecretVersions(rc.Printer, resp.Msg.GetVersion())
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (env: ORANGE_WS_ID)")
	cmd.Flags().StringVar(&name, "name", "", "secret name as realm/secret-id (e.g. prod/db-password)")
	cmd.Flags().StringVar(&versionID, "version-id", "", "version ID to disable")
	return cmd
}

func newSecretRetireCmd() *cobra.Command {
	var workspaceID, name, versionID string
	var shred bool
	cmd := &cobra.Command{
		Use:          "retire",
		Short:        "Retire a secret version (permanent; use --shred to zero material)",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if workspaceID == "" {
				workspaceID = os.Getenv("ORANGE_WS_ID")
			}
			if workspaceID == "" {
				return fmt.Errorf("--workspace-id is required (or set ORANGE_WS_ID)")
			}
			if name == "" {
				return fmt.Errorf("--name is required (format: realm/secret-id or just secret-id)")
			}
			if versionID == "" {
				return fmt.Errorf("--version-id is required")
			}
			realm, secretID := parseSecretName(name)
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.RetireVersion(context.Background(), connect.NewRequest(&secretv1.RetireVersionRequest{
				WorkspaceId: workspaceID,
				Realm:       realm,
				SecretId:    secretID,
				VersionId:   versionID,
				Shred:       shred,
			}))
			if err != nil {
				return err
			}
			return printSecretVersions(rc.Printer, resp.Msg.GetVersion())
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (env: ORANGE_WS_ID)")
	cmd.Flags().StringVar(&name, "name", "", "secret name as realm/secret-id (e.g. prod/db-password)")
	cmd.Flags().StringVar(&versionID, "version-id", "", "version ID to retire")
	cmd.Flags().BoolVar(&shred, "shred", false, "zero the material bytes after retiring")
	return cmd
}

// newSecretKEKCmd groups KEK management under "orange secret kek".
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
	var workspaceID, realm string
	cmd := &cobra.Command{
		Use:          "create",
		Short:        "Provision a service KEK (pool member or per-boundary)",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := secretconnect.NewSecretAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.CreateServiceKEK(context.Background(), connect.NewRequest(&secretv1.CreateServiceKEKRequest{
				WorkspaceId: workspaceID,
				Realm:       realm,
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
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (omit for pool member)")
	cmd.Flags().StringVar(&realm, "realm", "", "realm (omit for pool member)")
	return cmd
}

func newSecretKEKRotateCmd() *cobra.Command {
	var workspaceID, realm string
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
				WorkspaceId: workspaceID,
				Realm:       realm,
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
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (omit for pool rotation)")
	cmd.Flags().StringVar(&realm, "realm", "", "realm (omit for pool rotation)")
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
			rows[i] = fmt.Sprintf("%s/%s\t%s",
				s.GetRealm(), s.GetSecretId(), s.GetWorkspaceId())
		}
		p.Table("NAME\tWORKSPACE-ID", rows)
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
			name := v.GetRealm() + "/" + v.GetSecretId()
			rows[i] = fmt.Sprintf("%s\t%s\t%s\t%s\t%s",
				v.GetVersionId(), name, state, v.GetChecksum(), age(v.GetCreatedAt()))
		}
		p.Table("VERSION-ID\tNAME\tSTATE\tCHECKSUM\tAGE", rows)
		return nil
	}
}
