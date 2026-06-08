package server

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	adminv1 "github.com/dio/transit/examples/orange/api/orange/config/admin/v1"
	adminv1connect "github.com/dio/transit/examples/orange/api/orange/config/admin/v1/adminv1connect"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage workspace config snapshots",
	}
	cmd.AddCommand(newConfigPublishCmd())
	cmd.AddCommand(newConfigListCmd())
	return cmd
}

// newConfigPublishCmd implements:
//
//	orange admin config publish --workspace-id=<id> --file=config.yaml
//
// It reads a YAML config file and calls ConfigAdminService.PublishSnapshot.
// The server compiles the YAML, stores the resulting snapshot in
// config_snapshots, and broadcasts it to connected egress proxies via Watch.
// After this call, SnapshotService.Fetch for the workspace returns the snapshot
// instead of NotFound.
//
// The minimal YAML that produces a valid snapshot is a single LLM provider:
//
//	llm:
//	  providers:
//	    anthropic:
//	      kind: anthropic
//	      endpoint: https://api.anthropic.com
//	      auth:
//	        type: anthropic
//	        secret_ref: env://ANTHROPIC_API_KEY
//	  models:
//	    claude-3-5-sonnet:
//	      provider: anthropic
func newConfigPublishCmd() *cobra.Command {
	var (
		workspaceID string
		file        string
		publishedBy string
	)
	cmd := &cobra.Command{
		Use:          "publish",
		Short:        "Compile and publish a YAML config snapshot for a workspace",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if workspaceID == "" {
				workspaceID = os.Getenv("ORANGE_WS_ID")
			}
			if workspaceID == "" {
				return fmt.Errorf("--workspace-id is required (or set ORANGE_WS_ID)")
			}
			if file == "" {
				return fmt.Errorf("--file is required")
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return fmt.Errorf("read %s: %w", file, err)
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			by := publishedBy
			if by == "" {
				by = "orange-cli"
			}
			client := adminv1connect.NewConfigAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.PublishSnapshot(context.Background(), connect.NewRequest(&adminv1.PublishSnapshotRequest{
				WorkspaceId: workspaceID,
				YamlConfig:  string(data),
				PublishedBy: by,
			}))
			if err != nil {
				return err
			}
			return printSnapshotMeta(rc.Printer, resp.Msg.GetSnapshot())
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (env: ORANGE_WS_ID)")
	cmd.Flags().StringVar(&file, "file", "", "path to the YAML config file (required)")
	cmd.Flags().StringVar(&publishedBy, "by", "", "label identifying the publisher (default: orange-cli)")
	return cmd
}

// newConfigListCmd implements:
//
//	orange admin config list --workspace-id=<id>
//
// Lists published snapshots in descending version order so you can see what
// the server is currently serving and audit the publish history.
func newConfigListCmd() *cobra.Command {
	var workspaceID string
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "List published config snapshots for a workspace",
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
			client := adminv1connect.NewConfigAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.ListSnapshots(context.Background(), connect.NewRequest(&adminv1.ListSnapshotsRequest{
				WorkspaceId: workspaceID,
			}))
			if err != nil {
				return err
			}
			snaps := resp.Msg.GetSnapshots()
			switch rc.Printer.Format {
			case FormatJSON, FormatYAML:
				msgs := make([]proto.Message, len(snaps))
				for i, s := range snaps {
					msgs[i] = s
				}
				if len(msgs) == 1 {
					return rc.Printer.Proto(msgs[0])
				}
				return rc.Printer.ProtoList(msgs)
			default:
				rows := make([]string, len(snaps))
				for i, s := range snaps {
					ok := "ok"
					if !s.GetCompiledOk() {
						ok = "FAIL"
					}
					rows[i] = fmt.Sprintf("%d\t%s\t%s/%s\t%d B\t%s\t%s",
						s.GetVersion(),
						ok,
						s.GetFormat(), s.GetCompression(),
						s.GetByteSize(),
						s.GetCreatedBy(),
						age(s.GetCreatedAt()),
					)
				}
				rc.Printer.Table("VERSION\tSTATUS\tFORMAT\tSIZE\tBY\tAGE", rows)
				return nil
			}
		},
	}
	cmd.Flags().StringVar(&workspaceID, "workspace-id", "", "workspace ID (env: ORANGE_WS_ID)")
	return cmd
}

// cmdConfig routes config REPL subcommands.
//
//	config ls [<ws-id>]
//	config publish <file-path> [ws=<id>] [by=<who>]
func (s *replState) cmdConfig(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := adminv1connect.NewConfigAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "ls", "list":
		wsID := s.wsID
		if len(args) > 1 && !containsEq(args[1]) {
			wsID = args[1]
		}
		if wsID == "" {
			return fmt.Errorf("no workspace in context — provide ws-id or run 'use ws <id>'")
		}
		resp, err := client.ListSnapshots(ctx, connect.NewRequest(&adminv1.ListSnapshotsRequest{WorkspaceId: wsID}))
		if err != nil {
			return err
		}
		snaps := resp.Msg.GetSnapshots()
		rows := make([]string, len(snaps))
		for i, snap := range snaps {
			ok := "ok"
			if !snap.GetCompiledOk() {
				ok = "FAIL"
			}
			rows[i] = fmt.Sprintf("%d\t%s\t%s/%s\t%d B\t%s\t%s",
				snap.GetVersion(), ok,
				snap.GetFormat(), snap.GetCompression(),
				snap.GetByteSize(), snap.GetCreatedBy(),
				age(snap.GetCreatedAt()),
			)
		}
		s.rc.Printer.Table("VERSION\tSTATUS\tFORMAT\tSIZE\tBY\tAGE", rows)

	case "publish":
		if len(args) < 2 {
			return fmt.Errorf("usage: config publish <file-path> [ws=<id>] [by=<who>]")
		}
		filePath := args[1]
		wsID := kvGet(args[2:], "ws")
		if wsID == "" {
			wsID = s.wsID
		}
		if wsID == "" {
			return fmt.Errorf("no workspace in context — provide ws=<id> or run 'use ws <id>'")
		}
		by := kvGet(args[2:], "by")
		if by == "" {
			by = "orange-repl"
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("read %s: %w", filePath, err)
		}
		resp, err := client.PublishSnapshot(ctx, connect.NewRequest(&adminv1.PublishSnapshotRequest{
			WorkspaceId: wsID,
			YamlConfig:  string(data),
			PublishedBy: by,
		}))
		if err != nil {
			return err
		}
		return printSnapshotMeta(s.rc.Printer, resp.Msg.GetSnapshot())

	default:
		return fmt.Errorf("unknown config subcommand %q — try: ls [ws-id], publish <file-path> [ws=<id>] [by=<who>]", sub)
	}
	return nil
}

// containsEq reports whether s contains '=', used to distinguish positional
// args from key=value pairs in REPL input.
func containsEq(s string) bool {
	for _, c := range s {
		if c == '=' {
			return true
		}
	}
	return false
}

func printSnapshotMeta(p *Printer, s *adminv1.SnapshotMeta) error {
	switch p.Format {
	case FormatJSON, FormatYAML:
		return p.Proto(s)
	default:
		ok := "ok"
		if !s.GetCompiledOk() {
			ok = "FAIL"
			if s.CompileError != nil {
				ok += ": " + *s.CompileError
			}
		}
		p.Table("VERSION\tSTATUS\tFORMAT\tSIZE\tBY", []string{
			fmt.Sprintf("%d\t%s\t%s/%s\t%d B\t%s",
				s.GetVersion(), ok,
				s.GetFormat(), s.GetCompression(),
				s.GetByteSize(), s.GetCreatedBy(),
			),
		})
		return nil
	}
}
