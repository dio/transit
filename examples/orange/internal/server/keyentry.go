package server

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"github.com/chzyer/readline"

	keyentryv1 "github.com/dio/transit/examples/orange/api/orange/keyentry/admin/v1"
	keyentryconnect "github.com/dio/transit/examples/orange/api/orange/keyentry/admin/v1/adminv1connect"
)

// ── cobra: keyentry (token slot CRUD) ─────────────────────────────────────────

func newKeyEntryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keyentry",
		Short: "Manage token slots (key entries) within a workspace",
	}
	cmd.AddCommand(
		newKeyEntryListCmd(),
		newKeyEntryGetCmd(),
		newKeyEntryCreateCmd(),
		newKeyEntryUpdateCmd(),
		newKeyEntryDeleteCmd(),
	)
	return cmd
}

func newKeyEntryListCmd() *cobra.Command {
	var wsID string
	cmd := &cobra.Command{
		Use:          "ls",
		Short:        "List token slots in a workspace",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if wsID == "" {
				wsID = os.Getenv("ORANGE_WS_ID")
			}
			if wsID == "" {
				return fmt.Errorf("--ws is required (or set ORANGE_WS_ID)")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.ListKeys(context.Background(), connect.NewRequest(&keyentryv1.ListKeysRequest{WorkspaceId: wsID}))
			if err != nil {
				return err
			}
			return printKeyEntries(rc.Printer, resp.Msg.GetKeys()...)
		},
	}
	cmd.Flags().StringVar(&wsID, "ws", "", "workspace ID (env: ORANGE_WS_ID)")
	return cmd
}

func newKeyEntryGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "get <key-entry-id>",
		Short:        "Get a token slot",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.GetKey(context.Background(), connect.NewRequest(&keyentryv1.GetKeyRequest{KeyEntryId: args[0]}))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetKey())
		},
	}
	return cmd
}

func newKeyEntryCreateCmd() *cobra.Command {
	var wsID, userID, description string
	cmd := &cobra.Command{
		Use:          "create <name>",
		Short:        "Create a token slot (key entry)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			if wsID == "" {
				wsID = os.Getenv("ORANGE_WS_ID")
			}
			if wsID == "" {
				return fmt.Errorf("--ws is required (or set ORANGE_WS_ID)")
			}
			if userID == "" {
				return fmt.Errorf("--user is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			req := &keyentryv1.CreateKeyRequest{
				WorkspaceId: wsID,
				UserId:      userID,
				Name:        args[0],
			}
			if description != "" {
				req.Description = &description
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.CreateKey(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetKey())
		},
	}
	cmd.Flags().StringVar(&wsID, "ws", "", "workspace ID (env: ORANGE_WS_ID)")
	cmd.Flags().StringVar(&userID, "user", "", "user ID that owns this token slot")
	cmd.Flags().StringVar(&description, "description", "", "optional description")
	return cmd
}

func newKeyEntryUpdateCmd() *cobra.Command {
	var description string
	cmd := &cobra.Command{
		Use:          "update <key-entry-id>",
		Short:        "Update a token slot's description",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			req := &keyentryv1.UpdateKeyRequest{KeyEntryId: args[0]}
			if description != "" {
				req.Description = &description
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.UpdateKey(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetKey())
		},
	}
	cmd.Flags().StringVar(&description, "description", "", "new description")
	return cmd
}

func newKeyEntryDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "delete <key-entry-id>",
		Short:        "Delete a token slot",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			_, err = client.DeleteKey(context.Background(), connect.NewRequest(&keyentryv1.DeleteKeyRequest{KeyEntryId: args[0]}))
			if err != nil {
				return err
			}
			rc.Printer.OK("deleted")
			return nil
		},
	}
	return cmd
}

// ── cobra: keyentry-token (token lifecycle) ─────────────────────────────────────────

func newKeyEntryTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keyentry-token",
		Short: "Manage PASETO tokens issued from a token slot",
	}
	cmd.AddCommand(
		newKeyEntryTokenListCmd(),
		newKeyEntryTokenGetCmd(),
		newKeyEntryTokenIssueCmd(),
		newKeyEntryTokenRevokeCmd(),
	)
	return cmd
}

func newKeyEntryTokenListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "ls <key-entry-id>",
		Short:        "List tokens issued from a token slot",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.ListTokens(context.Background(), connect.NewRequest(&keyentryv1.ListTokensRequest{KeyEntryId: args[0]}))
			if err != nil {
				return err
			}
			return printKeyEntryTokens(rc.Printer, resp.Msg.GetTokens()...)
		},
	}
	return cmd
}

func newKeyEntryTokenGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "get <token-id>",
		Short:        "Get a PASETO token record",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.GetToken(context.Background(), connect.NewRequest(&keyentryv1.GetTokenRequest{TokenId: args[0]}))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetToken())
		},
	}
	return cmd
}

func newKeyEntryTokenIssueCmd() *cobra.Command {
	var ttl int64
	cmd := &cobra.Command{
		Use:          "issue <key-entry-id>",
		Short:        "Issue an anonymous PASETO token from a token slot",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			req := &keyentryv1.IssueTokenRequest{
				KeyEntryId: args[0],
				TtlSeconds: ttl,
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.IssueToken(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			rc.Printer.Table("TOKEN (shown once)\tEXPIRES", []string{
				fmt.Sprintf("%s\t%s", resp.Msg.GetToken(), ttlString(resp.Msg.GetMetadata().GetExp().AsTime())),
			})
			return nil
		},
	}
	cmd.Flags().Int64Var(&ttl, "ttl", 0, "token TTL in seconds (0 = no expiry)")
	return cmd
}

func newKeyEntryTokenRevokeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "revoke <token-id>",
		Short:        "Revoke a PASETO token",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			_, err = client.RevokeToken(context.Background(), connect.NewRequest(&keyentryv1.RevokeTokenRequest{TokenId: args[0]}))
			if err != nil {
				return err
			}
			rc.Printer.OK("revoked")
			return nil
		},
	}
	return cmd
}

// ── cobra: keyentry-secret (key secret CRUD) ────────────────────────────────────────

func newKeyEntrySecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keyentry-secret",
		Short: "Manage upstream API secrets attached to a token slot",
	}
	cmd.AddCommand(
		newKeyEntrySecretListCmd(),
		newKeyEntrySecretGetCmd(),
		newKeyEntrySecretCreateCmd(),
		newKeyEntrySecretRotateCmd(),
		newKeyEntrySecretDeleteCmd(),
	)
	return cmd
}

func newKeyEntrySecretListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "ls <key-entry-id>",
		Short:        "List key secrets for a token slot",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.ListKeySecrets(context.Background(), connect.NewRequest(&keyentryv1.ListKeySecretsRequest{KeyEntryId: args[0]}))
			if err != nil {
				return err
			}
			return printKeyEntrySecrets(rc.Printer, resp.Msg.GetSecrets()...)
		},
	}
	return cmd
}

func newKeyEntrySecretGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "get <key-secret-id>",
		Short:        "Get a key secret record",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.GetKeySecret(context.Background(), connect.NewRequest(&keyentryv1.GetKeySecretRequest{KeySecretId: args[0]}))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetSecret())
		},
	}
	return cmd
}

func newKeyEntrySecretCreateCmd() *cobra.Command {
	var target, description string
	cmd := &cobra.Command{
		Use:          "create <key-entry-id>",
		Short:        "Create a key secret (prompts for the secret value)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			if target == "" {
				return fmt.Errorf("--target is required (e.g. openai, anthropic)")
			}
			value, err := promptHiddenValue("Secret value (hidden): ")
			if err != nil {
				return err
			}
			if len(value) == 0 {
				return fmt.Errorf("value is empty; nothing stored")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			req := &keyentryv1.CreateKeySecretRequest{
				KeyEntryId:      args[0],
				UpstreamTarget:  target,
				Value:           string(value),
			}
			if description != "" {
				req.Description = &description
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.CreateKeySecret(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetSecret())
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "upstream target identifier (e.g. openai, anthropic)")
	cmd.Flags().StringVar(&description, "description", "", "optional description")
	return cmd
}

func newKeyEntrySecretRotateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "rotate <key-secret-id>",
		Short:        "Rotate a key secret (prompts for the new value)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			value, err := promptHiddenValue("New secret value (hidden): ")
			if err != nil {
				return err
			}
			if len(value) == 0 {
				return fmt.Errorf("value is empty; nothing stored")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.RotateKeySecret(context.Background(), connect.NewRequest(&keyentryv1.RotateKeySecretRequest{
				KeySecretId: args[0],
				Value:       string(value),
			}))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetSecret())
		},
	}
	return cmd
}

func newKeyEntrySecretDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "delete <key-secret-id>",
		Short:        "Delete a key secret",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := keyentryconnect.NewKeyEntryAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			_, err = client.DeleteKeySecret(context.Background(), connect.NewRequest(&keyentryv1.DeleteKeySecretRequest{KeySecretId: args[0]}))
			if err != nil {
				return err
			}
			rc.Printer.OK("deleted")
			return nil
		},
	}
	return cmd
}

// ── REPL: keyentry ────────────────────────────────────────────────────────────

// cmdKeyEntry routes keyentry REPL subcommands. ws context used for ls/create.
//
//	keyentry ls [<ws-id>]
//	keyentry get <key-entry-id>
//	keyentry create <name> [user=<id>] [ws=<id>] [desc=<text>]
//	keyentry update <key-entry-id> [desc=<text>]
//	keyentry delete <key-entry-id>
func (s *replState) cmdKeyEntry(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := keyentryconnect.NewKeyEntryAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "ls", "list":
		wsID := s.wsID
		if len(args) > 1 && !containsEq(args[1]) {
			wsID = args[1]
		}
		if wsID == "" {
			return fmt.Errorf("no workspace in context — provide ws-id or run 'use ws <id>'")
		}
		resp, err := client.ListKeys(ctx, connect.NewRequest(&keyentryv1.ListKeysRequest{WorkspaceId: wsID}))
		if err != nil {
			return err
		}
		return printKeyEntries(s.rc.Printer, resp.Msg.GetKeys()...)

	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry get <key-entry-id>")
		}
		resp, err := client.GetKey(ctx, connect.NewRequest(&keyentryv1.GetKeyRequest{KeyEntryId: args[1]}))
		if err != nil {
			return err
		}
		return s.rc.Printer.Proto(resp.Msg.GetKey())

	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry create <name> [user=<id>] [ws=<id>] [desc=<text>]")
		}
		name := args[1]
		wsID := kvGet(args[2:], "ws")
		if wsID == "" {
			wsID = s.wsID
		}
		if wsID == "" {
			return fmt.Errorf("no workspace in context — provide ws=<id> or run 'use ws <id>'")
		}
		userID := kvGet(args[2:], "user")
		if userID == "" {
			return fmt.Errorf("user=<id> is required")
		}
		desc := kvGet(args[2:], "desc")
		req := &keyentryv1.CreateKeyRequest{
			WorkspaceId: wsID,
			UserId:      userID,
			Name:        name,
		}
		if desc != "" {
			req.Description = &desc
		}
		resp, err := client.CreateKey(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return s.rc.Printer.Proto(resp.Msg.GetKey())

	case "update":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry update <key-entry-id> [desc=<text>]")
		}
		req := &keyentryv1.UpdateKeyRequest{KeyEntryId: args[1]}
		if desc := kvGet(args[2:], "desc"); desc != "" {
			req.Description = &desc
		}
		resp, err := client.UpdateKey(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return s.rc.Printer.Proto(resp.Msg.GetKey())

	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry delete <key-entry-id>")
		}
		_, err := client.DeleteKey(ctx, connect.NewRequest(&keyentryv1.DeleteKeyRequest{KeyEntryId: args[1]}))
		if err != nil {
			return err
		}
		s.rc.Printer.OK("deleted")

	default:
		return fmt.Errorf("unknown keyentry subcommand %q — try: ls, get, create, update, delete", sub)
	}
	return nil
}

// ── REPL: keyentry-token ─────────────────────────────────────────────────────────────

// cmdKeyEntryToken routes ketoken REPL subcommands.
//
//	keyentry-token ls <key-entry-id>
//	keyentry-token get <token-id>
//	keyentry-token issue <key-entry-id> [ttl=<seconds>]
//	keyentry-token revoke <token-id>
func (s *replState) cmdKeyEntryToken(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := keyentryconnect.NewKeyEntryAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "ls", "list":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-token ls <key-entry-id>")
		}
		resp, err := client.ListTokens(ctx, connect.NewRequest(&keyentryv1.ListTokensRequest{KeyEntryId: args[1]}))
		if err != nil {
			return err
		}
		return printKeyEntryTokens(s.rc.Printer, resp.Msg.GetTokens()...)

	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-token get <token-id>")
		}
		resp, err := client.GetToken(ctx, connect.NewRequest(&keyentryv1.GetTokenRequest{TokenId: args[1]}))
		if err != nil {
			return err
		}
		return s.rc.Printer.Proto(resp.Msg.GetToken())

	case "issue":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-token issue <key-entry-id> [ttl=<seconds>]")
		}
		var ttl int64
		fmt.Sscanf(kvGet(args[2:], "ttl"), "%d", &ttl)
		req := &keyentryv1.IssueTokenRequest{
			KeyEntryId: args[1],
			TtlSeconds: ttl,
		}
		resp, err := client.IssueToken(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		s.rc.Printer.Table("TOKEN (shown once)\tEXPIRES", []string{
			fmt.Sprintf("%s\t%s", resp.Msg.GetToken(), ttlString(resp.Msg.GetMetadata().GetExp().AsTime())),
		})

	case "revoke":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-token revoke <token-id>")
		}
		_, err := client.RevokeToken(ctx, connect.NewRequest(&keyentryv1.RevokeTokenRequest{TokenId: args[1]}))
		if err != nil {
			return err
		}
		s.rc.Printer.OK("revoked")

	default:
		return fmt.Errorf("unknown keyentry-token subcommand %q — try: ls, get, issue, revoke", sub)
	}
	return nil
}

// ── REPL: keyentry-secret ────────────────────────────────────────────────────────────

// cmdKeyEntrySecret routes kesecret REPL subcommands.
//
//	keyentry-secret ls <key-entry-id>
//	keyentry-secret get <key-secret-id>
//	keyentry-secret create <key-entry-id> target=<upstream> [desc=<text>]   (prompts for value)
//	keyentry-secret rotate <key-secret-id>                                   (prompts for value)
//	keyentry-secret delete <key-secret-id>
func (s *replState) cmdKeyEntrySecret(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := keyentryconnect.NewKeyEntryAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "ls", "list":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-secret ls <key-entry-id>")
		}
		resp, err := client.ListKeySecrets(ctx, connect.NewRequest(&keyentryv1.ListKeySecretsRequest{KeyEntryId: args[1]}))
		if err != nil {
			return err
		}
		return printKeyEntrySecrets(s.rc.Printer, resp.Msg.GetSecrets()...)

	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-secret get <key-secret-id>")
		}
		resp, err := client.GetKeySecret(ctx, connect.NewRequest(&keyentryv1.GetKeySecretRequest{KeySecretId: args[1]}))
		if err != nil {
			return err
		}
		return s.rc.Printer.Proto(resp.Msg.GetSecret())

	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-secret create <key-entry-id> target=<upstream> [desc=<text>]")
		}
		target := kvGet(args[2:], "target")
		if target == "" {
			return fmt.Errorf("target=<upstream> is required (e.g. target=openai)")
		}
		value, err := s.readHiddenInput("Secret value (hidden): ")
		if err != nil {
			return err
		}
		if len(value) == 0 {
			return fmt.Errorf("value is empty; nothing stored")
		}
		desc := kvGet(args[2:], "desc")
		req := &keyentryv1.CreateKeySecretRequest{
			KeyEntryId:     args[1],
			UpstreamTarget: target,
			Value:          string(value),
		}
		if desc != "" {
			req.Description = &desc
		}
		resp, err := client.CreateKeySecret(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return s.rc.Printer.Proto(resp.Msg.GetSecret())

	case "rotate":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-secret rotate <key-secret-id>")
		}
		value, err := s.readHiddenInput("New secret value (hidden): ")
		if err != nil {
			return err
		}
		if len(value) == 0 {
			return fmt.Errorf("value is empty; nothing stored")
		}
		resp, err := client.RotateKeySecret(ctx, connect.NewRequest(&keyentryv1.RotateKeySecretRequest{
			KeySecretId: args[1],
			Value:       string(value),
		}))
		if err != nil {
			return err
		}
		return s.rc.Printer.Proto(resp.Msg.GetSecret())

	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: keyentry-secret delete <key-secret-id>")
		}
		_, err := client.DeleteKeySecret(ctx, connect.NewRequest(&keyentryv1.DeleteKeySecretRequest{KeySecretId: args[1]}))
		if err != nil {
			return err
		}
		s.rc.Printer.OK("deleted")

	default:
		return fmt.Errorf("unknown keyentry-secret subcommand %q — try: ls, get, create, rotate, delete", sub)
	}
	return nil
}

// ── print helpers ─────────────────────────────────────────────────────────────

func printKeyEntries(p *Printer, keys ...*keyentryv1.Key) error {
	if p.Format != FormatTable {
		for _, k := range keys {
			if err := p.Proto(k); err != nil {
				return err
			}
		}
		return nil
	}
	rows := make([]string, len(keys))
	for i, k := range keys {
		rows[i] = fmt.Sprintf("%s\t%s\t%s\t%s\t%s",
			shortID(k.GetKeyEntryId()),
			k.GetName(),
			shortID(k.GetUserId()),
			shortID(k.GetWorkspaceId()),
			age(k.GetCreatedAt()),
		)
	}
	p.Table("KEY-ENTRY-ID\tNAME\tUSER-ID\tWORKSPACE-ID\tAGE", rows)
	return nil
}

func printKeyEntryTokens(p *Printer, tokens ...*keyentryv1.PASETOToken) error {
	if p.Format != FormatTable {
		for _, t := range tokens {
			if err := p.Proto(t); err != nil {
				return err
			}
		}
		return nil
	}
	rows := make([]string, len(tokens))
	for i, t := range tokens {
		revoked := "active"
		if t.GetRevoked() {
			revoked = "revoked"
		}
		exp := "never"
		if e := t.GetExp(); e != nil && e.AsTime().Year() < 9999 {
			exp = ttlString(e.AsTime())
		}
		rows[i] = fmt.Sprintf("%s\t%s\t%s\t%s\t%s",
			shortID(t.GetTokenId()),
			shortID(t.GetKeyEntryId()),
			revoked,
			exp,
			age(t.GetCreatedAt()),
		)
	}
	p.Table("TOKEN-ID\tKEY-ENTRY-ID\tSTATUS\tEXPIRES\tAGE", rows)
	return nil
}

func printKeyEntrySecrets(p *Printer, secrets ...*keyentryv1.KeySecret) error {
	if p.Format != FormatTable {
		for _, s := range secrets {
			if err := p.Proto(s); err != nil {
				return err
			}
		}
		return nil
	}
	rows := make([]string, len(secrets))
	for i, s := range secrets {
		active := "inactive"
		if s.GetActive() {
			active = "active"
		}
		rows[i] = fmt.Sprintf("%s\t%s\t%s\t%d\t%s\t%s",
			shortID(s.GetKeySecretId()),
			shortID(s.GetKeyEntryId()),
			s.GetUpstreamTarget(),
			s.GetVersion(),
			active,
			age(s.GetCreatedAt()),
		)
	}
	p.Table("KEY-SECRET-ID\tKEY-ENTRY-ID\tTARGET\tVERSION\tSTATUS\tAGE", rows)
	return nil
}

// ── shared helpers ────────────────────────────────────────────────────────────

// promptHiddenValue reads a secret value with echo disabled from a new readline
// instance — used in cobra one-shot commands that have no REPL state.
func promptHiddenValue(prompt string) ([]byte, error) {
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          prompt,
		InterruptPrompt: "^C",
		EOFPrompt:       "",
	})
	if err != nil {
		return nil, err
	}
	defer rl.Close()
	return rl.ReadPassword(prompt)
}
