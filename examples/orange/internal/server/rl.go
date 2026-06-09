package server

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	adminv1 "github.com/dio/transit/examples/orange/api/orange/config/admin/v1"
	configconnect "github.com/dio/transit/examples/orange/api/orange/config/admin/v1/adminv1connect"
)

// ── cobra: rl-tier ────────────────────────────────────────────────────────────

func newRLTierCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rl-tier",
		Short: "Manage rate-limit tiers",
	}
	cmd.AddCommand(
		newRLTierListCmd(),
		newRLTierGetCmd(),
		newRLTierCreateCmd(),
		newRLTierUpdateCmd(),
		newRLTierDeleteCmd(),
	)
	return cmd
}

func newRLTierListCmd() *cobra.Command {
	var wsID string
	cmd := &cobra.Command{
		Use:          "ls",
		Short:        "List rate-limit tiers for a workspace",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if wsID == "" {
				return fmt.Errorf("--ws is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := configconnect.NewConfigAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.ListRateLimitTiers(context.Background(), connect.NewRequest(&adminv1.ListRateLimitTiersRequest{
				WorkspaceId: wsID,
			}))
			if err != nil {
				return err
			}
			return printRLTiers(rc.Printer, resp.Msg.GetTiers()...)
		},
	}
	cmd.Flags().StringVar(&wsID, "ws", "", "workspace ID")
	return cmd
}

func newRLTierGetCmd() *cobra.Command {
	var wsID string
	cmd := &cobra.Command{
		Use:          "get <name>",
		Short:        "Get a rate-limit tier",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			if wsID == "" {
				return fmt.Errorf("--ws is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := configconnect.NewConfigAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.GetRateLimitTier(context.Background(), connect.NewRequest(&adminv1.GetRateLimitTierRequest{
				WorkspaceId: wsID,
				Name:        args[0],
			}))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetTier())
		},
	}
	cmd.Flags().StringVar(&wsID, "ws", "", "workspace ID")
	return cmd
}

// tierFlags groups the many numeric fields for create/update.
type tierFlags struct {
	rpm, rph, rpd                       int32
	inputTPM, inputTPH, inputTPD        int32
	outputTPM, outputTPH, outputTPD     int32
	cacheReadTPH, cacheReadTPD          int32
	cacheWriteTPH, cacheWriteTPD        int32
	usdPerMinute, usdPerHour, usdPerDay float64
	onExceed                            string
}

func addTierFlags(cmd *cobra.Command, tf *tierFlags) {
	f := cmd.Flags()
	f.Int32Var(&tf.rpm, "rpm", 0, "requests per minute (0 = unlimited)")
	f.Int32Var(&tf.rph, "rph", 0, "requests per hour")
	f.Int32Var(&tf.rpd, "rpd", 0, "requests per day")
	f.Int32Var(&tf.inputTPM, "input-tpm", 0, "input tokens per minute")
	f.Int32Var(&tf.inputTPH, "input-tph", 0, "input tokens per hour")
	f.Int32Var(&tf.inputTPD, "input-tpd", 0, "input tokens per day")
	f.Int32Var(&tf.outputTPM, "output-tpm", 0, "output tokens per minute")
	f.Int32Var(&tf.outputTPH, "output-tph", 0, "output tokens per hour")
	f.Int32Var(&tf.outputTPD, "output-tpd", 0, "output tokens per day")
	f.Int32Var(&tf.cacheReadTPH, "cache-read-tph", 0, "cache-read tokens per hour")
	f.Int32Var(&tf.cacheReadTPD, "cache-read-tpd", 0, "cache-read tokens per day")
	f.Int32Var(&tf.cacheWriteTPH, "cache-write-tph", 0, "cache-write tokens per hour")
	f.Int32Var(&tf.cacheWriteTPD, "cache-write-tpd", 0, "cache-write tokens per day")
	f.Float64Var(&tf.usdPerMinute, "usd-per-minute", 0, "USD cost cap per minute")
	f.Float64Var(&tf.usdPerHour, "usd-per-hour", 0, "USD cost cap per hour")
	f.Float64Var(&tf.usdPerDay, "usd-per-day", 0, "USD cost cap per day")
	f.StringVar(&tf.onExceed, "on-exceed", "reject", "action on limit breach: reject | throttle | log_only")
}

func tierFlagsToCreateReq(wsID, name string, tf tierFlags) *adminv1.CreateRateLimitTierRequest {
	return &adminv1.CreateRateLimitTierRequest{
		WorkspaceId:             wsID,
		Name:                    name,
		Rpm:                     tf.rpm,
		Rph:                     tf.rph,
		Rpd:                     tf.rpd,
		InputTokensPerMinute:    tf.inputTPM,
		InputTokensPerHour:      tf.inputTPH,
		InputTokensPerDay:       tf.inputTPD,
		OutputTokensPerMinute:   tf.outputTPM,
		OutputTokensPerHour:     tf.outputTPH,
		OutputTokensPerDay:      tf.outputTPD,
		CacheReadTokensPerHour:  tf.cacheReadTPH,
		CacheReadTokensPerDay:   tf.cacheReadTPD,
		CacheWriteTokensPerHour: tf.cacheWriteTPH,
		CacheWriteTokensPerDay:  tf.cacheWriteTPD,
		UsdPerMinute:            tf.usdPerMinute,
		UsdPerHour:              tf.usdPerHour,
		UsdPerDay:               tf.usdPerDay,
		OnExceed:                onExceedProtoFromString(tf.onExceed),
	}
}

func tierFlagsToUpdateReq(wsID, name string, tf tierFlags) *adminv1.UpdateRateLimitTierRequest {
	return &adminv1.UpdateRateLimitTierRequest{
		WorkspaceId:             wsID,
		Name:                    name,
		Rpm:                     tf.rpm,
		Rph:                     tf.rph,
		Rpd:                     tf.rpd,
		InputTokensPerMinute:    tf.inputTPM,
		InputTokensPerHour:      tf.inputTPH,
		InputTokensPerDay:       tf.inputTPD,
		OutputTokensPerMinute:   tf.outputTPM,
		OutputTokensPerHour:     tf.outputTPH,
		OutputTokensPerDay:      tf.outputTPD,
		CacheReadTokensPerHour:  tf.cacheReadTPH,
		CacheReadTokensPerDay:   tf.cacheReadTPD,
		CacheWriteTokensPerHour: tf.cacheWriteTPH,
		CacheWriteTokensPerDay:  tf.cacheWriteTPD,
		UsdPerMinute:            tf.usdPerMinute,
		UsdPerHour:              tf.usdPerHour,
		UsdPerDay:               tf.usdPerDay,
		OnExceed:                onExceedProtoFromString(tf.onExceed),
	}
}

func newRLTierCreateCmd() *cobra.Command {
	var wsID string
	var tf tierFlags
	cmd := &cobra.Command{
		Use:          "create <name>",
		Short:        "Create a rate-limit tier",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			if wsID == "" {
				return fmt.Errorf("--ws is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := configconnect.NewConfigAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.CreateRateLimitTier(context.Background(), connect.NewRequest(tierFlagsToCreateReq(wsID, args[0], tf)))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetTier())
		},
	}
	cmd.Flags().StringVar(&wsID, "ws", "", "workspace ID")
	addTierFlags(cmd, &tf)
	return cmd
}

func newRLTierUpdateCmd() *cobra.Command {
	var wsID string
	var tf tierFlags
	cmd := &cobra.Command{
		Use:          "update <name>",
		Short:        "Update a rate-limit tier (full replacement of all limit fields)",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			if wsID == "" {
				return fmt.Errorf("--ws is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := configconnect.NewConfigAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.UpdateRateLimitTier(context.Background(), connect.NewRequest(tierFlagsToUpdateReq(wsID, args[0], tf)))
			if err != nil {
				return err
			}
			return rc.Printer.Proto(resp.Msg.GetTier())
		},
	}
	cmd.Flags().StringVar(&wsID, "ws", "", "workspace ID")
	addTierFlags(cmd, &tf)
	return cmd
}

func newRLTierDeleteCmd() *cobra.Command {
	var wsID string
	cmd := &cobra.Command{
		Use:          "delete <name>",
		Short:        "Delete a rate-limit tier",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			if wsID == "" {
				return fmt.Errorf("--ws is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := configconnect.NewConfigAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			_, err = client.DeleteRateLimitTier(context.Background(), connect.NewRequest(&adminv1.DeleteRateLimitTierRequest{
				WorkspaceId: wsID,
				Name:        args[0],
			}))
			if err != nil {
				return err
			}
			rc.Printer.OK("deleted")
			return nil
		},
	}
	cmd.Flags().StringVar(&wsID, "ws", "", "workspace ID")
	return cmd
}

// ── cobra: rl-scope ───────────────────────────────────────────────────────────

func newRLScopeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rl-scope",
		Short: "Manage rate-limit scope policies",
	}
	cmd.AddCommand(
		newRLScopeListCmd(),
		newRLScopeGetCmd(),
		newRLScopeSetCmd(),
		newRLScopeDeleteCmd(),
	)
	return cmd
}

func newRLScopeListCmd() *cobra.Command {
	var wsID string
	cmd := &cobra.Command{
		Use:          "ls",
		Short:        "List rate-limit scopes for a workspace",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if wsID == "" {
				return fmt.Errorf("--ws is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			client := configconnect.NewConfigAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.ListRateLimitScopes(context.Background(), connect.NewRequest(&adminv1.ListRateLimitScopesRequest{
				WorkspaceId: wsID,
			}))
			if err != nil {
				return err
			}
			return printRLScopes(rc.Printer, resp.Msg.GetScopes()...)
		},
	}
	cmd.Flags().StringVar(&wsID, "ws", "", "workspace ID")
	return cmd
}

func newRLScopeGetCmd() *cobra.Command {
	var wsID, user string
	cmd := &cobra.Command{
		Use:          "get",
		Short:        "Get a rate-limit scope (workspace-level or user-level)",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if wsID == "" {
				return fmt.Errorf("--ws is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			req := &adminv1.GetRateLimitScopeRequest{WorkspaceId: wsID}
			if user != "" {
				req.User = &user
			}
			client := configconnect.NewConfigAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.GetRateLimitScope(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return printRLScopeDetail(rc.Printer, resp.Msg.GetScope())
		},
	}
	cmd.Flags().StringVar(&wsID, "ws", "", "workspace ID")
	cmd.Flags().StringVar(&user, "user", "", "user ID for user-level scope (absent = workspace-level)")
	return cmd
}

func newRLScopeSetCmd() *cobra.Command {
	var wsID, user string
	var entries []string
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Set (full replace) rate-limit scope entries",
		Long: `Set the ordered list of policy entries for a workspace (or user) scope.

Each --entry value is a JSON object with fields matching RateLimitPolicyEntry:

  orange admin rl-scope set --ws <id> \
    --entry '{"models":["*"],"rpm":100,"on_exceed":"throttle"}' \
    --entry '{"models":["claude-3-opus"],"tier_name":"premium"}'

Omitting --user sets the workspace-level policy. Providing --user sets a
per-user override. The server replaces the entire entry list atomically.`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if wsID == "" {
				return fmt.Errorf("--ws is required")
			}
			protoEntries, err := parseEntryJSONSlice(entries)
			if err != nil {
				return err
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			req := &adminv1.SetRateLimitScopeRequest{
				WorkspaceId: wsID,
				Entries:     protoEntries,
			}
			if user != "" {
				req.User = &user
			}
			client := configconnect.NewConfigAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			resp, err := client.SetRateLimitScope(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			return printRLScopeDetail(rc.Printer, resp.Msg.GetScope())
		},
	}
	cmd.Flags().StringVar(&wsID, "ws", "", "workspace ID")
	cmd.Flags().StringVar(&user, "user", "", "user ID for user-level scope (absent = workspace-level)")
	cmd.Flags().StringArrayVar(&entries, "entry", nil, "JSON policy entry (repeatable, order matters)")
	return cmd
}

func newRLScopeDeleteCmd() *cobra.Command {
	var wsID, user string
	cmd := &cobra.Command{
		Use:          "delete",
		Short:        "Delete a rate-limit scope",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			if wsID == "" {
				return fmt.Errorf("--ws is required")
			}
			rc, err := resolveRunCtx()
			if err != nil {
				return err
			}
			req := &adminv1.DeleteRateLimitScopeRequest{WorkspaceId: wsID}
			if user != "" {
				req.User = &user
			}
			client := configconnect.NewConfigAdminServiceClient(rc.HTTPClient, rc.ServerURL, rc.ConnectOpts...)
			_, err = client.DeleteRateLimitScope(context.Background(), connect.NewRequest(req))
			if err != nil {
				return err
			}
			rc.Printer.OK("deleted")
			return nil
		},
	}
	cmd.Flags().StringVar(&wsID, "ws", "", "workspace ID")
	cmd.Flags().StringVar(&user, "user", "", "user ID for user-level scope (absent = workspace-level)")
	return cmd
}

// ── REPL: rl-tier ─────────────────────────────────────────────────────────────

// cmdRLTier routes rl-tier REPL subcommands. Requires ws context for most ops.
//
//	rl-tier ls [<ws-id>]
//	rl-tier get <name> [<ws-id>]
//	rl-tier create <name> [key=val ...]
//	rl-tier update <name> [key=val ...]
//	rl-tier delete <name> [<ws-id>]
func (s *replState) cmdRLTier(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := configconnect.NewConfigAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	wsID := s.wsID

	switch sub {
	case "ls", "list":
		if len(args) > 1 {
			wsID = args[1]
		}
		if wsID == "" {
			return fmt.Errorf("no workspace in context — provide ws-id or run 'use ws <id>'")
		}
		resp, err := client.ListRateLimitTiers(ctx, connect.NewRequest(&adminv1.ListRateLimitTiersRequest{WorkspaceId: wsID}))
		if err != nil {
			return err
		}
		return printRLTiers(s.rc.Printer, resp.Msg.GetTiers()...)

	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: rl-tier get <name> [ws-id]")
		}
		if len(args) > 2 {
			wsID = args[2]
		}
		if wsID == "" {
			return fmt.Errorf("no workspace in context — provide ws-id or run 'use ws <id>'")
		}
		resp, err := client.GetRateLimitTier(ctx, connect.NewRequest(&adminv1.GetRateLimitTierRequest{
			WorkspaceId: wsID,
			Name:        args[1],
		}))
		if err != nil {
			return err
		}
		return s.rc.Printer.Proto(resp.Msg.GetTier())

	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: rl-tier create <name> [rpm=N rph=N rpd=N usd_per_day=N on_exceed=reject|throttle|log_only ...]")
		}
		if wsID == "" {
			return fmt.Errorf("no workspace in context — run 'use ws <id>' first")
		}
		req := tierFlagsToCreateReq(wsID, args[1], parseTierKVArgs(args[2:]))
		resp, err := client.CreateRateLimitTier(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return s.rc.Printer.Proto(resp.Msg.GetTier())

	case "update":
		if len(args) < 2 {
			return fmt.Errorf("usage: rl-tier update <name> [rpm=N ...]")
		}
		if wsID == "" {
			return fmt.Errorf("no workspace in context — run 'use ws <id>' first")
		}
		req := tierFlagsToUpdateReq(wsID, args[1], parseTierKVArgs(args[2:]))
		resp, err := client.UpdateRateLimitTier(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return s.rc.Printer.Proto(resp.Msg.GetTier())

	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: rl-tier delete <name> [ws-id]")
		}
		if len(args) > 2 {
			wsID = args[2]
		}
		if wsID == "" {
			return fmt.Errorf("no workspace in context — provide ws-id or run 'use ws <id>'")
		}
		_, err := client.DeleteRateLimitTier(ctx, connect.NewRequest(&adminv1.DeleteRateLimitTierRequest{
			WorkspaceId: wsID,
			Name:        args[1],
		}))
		if err != nil {
			return err
		}
		s.rc.Printer.OK("deleted")

	default:
		return fmt.Errorf("unknown rl-tier subcommand %q — try: ls, get, create, update, delete", sub)
	}
	return nil
}

// ── REPL: rl-scope ────────────────────────────────────────────────────────────

// cmdRLScope routes rl-scope REPL subcommands.
//
//	rl-scope ls [<ws-id>]
//	rl-scope get [user=<id>]
//	rl-scope set [user=<id>] entry='<json>' [entry='<json>' ...]
//	rl-scope delete [user=<id>]
func (s *replState) cmdRLScope(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := configconnect.NewConfigAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	wsID := s.wsID

	switch sub {
	case "ls", "list":
		if len(args) > 1 {
			wsID = args[1]
		}
		if wsID == "" {
			return fmt.Errorf("no workspace in context — provide ws-id or run 'use ws <id>'")
		}
		resp, err := client.ListRateLimitScopes(ctx, connect.NewRequest(&adminv1.ListRateLimitScopesRequest{WorkspaceId: wsID}))
		if err != nil {
			return err
		}
		return printRLScopes(s.rc.Printer, resp.Msg.GetScopes()...)

	case "get":
		if wsID == "" {
			return fmt.Errorf("no workspace in context — run 'use ws <id>' first")
		}
		user := kvGet(args[1:], "user")
		req := &adminv1.GetRateLimitScopeRequest{WorkspaceId: wsID}
		if user != "" {
			req.User = &user
		}
		resp, err := client.GetRateLimitScope(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return printRLScopeDetail(s.rc.Printer, resp.Msg.GetScope())

	case "set":
		if wsID == "" {
			return fmt.Errorf("no workspace in context — run 'use ws <id>' first")
		}
		user := kvGet(args[1:], "user")
		entryJSONs := kvGetAll(args[1:], "entry")
		if len(entryJSONs) == 0 {
			return fmt.Errorf("provide at least one entry='<json>' argument\n" +
				"  example: rl-scope set entry='{\"models\":[\"*\"],\"rpm\":100}'")
		}
		protoEntries, err := parseEntryJSONSlice(entryJSONs)
		if err != nil {
			return err
		}
		req := &adminv1.SetRateLimitScopeRequest{WorkspaceId: wsID, Entries: protoEntries}
		if user != "" {
			req.User = &user
		}
		resp, err := client.SetRateLimitScope(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return printRLScopeDetail(s.rc.Printer, resp.Msg.GetScope())

	case "delete":
		if wsID == "" {
			return fmt.Errorf("no workspace in context — run 'use ws <id>' first")
		}
		user := kvGet(args[1:], "user")
		req := &adminv1.DeleteRateLimitScopeRequest{WorkspaceId: wsID}
		if user != "" {
			req.User = &user
		}
		_, err := client.DeleteRateLimitScope(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		s.rc.Printer.OK("deleted")

	default:
		return fmt.Errorf("unknown rl-scope subcommand %q — try: ls, get, set, delete", sub)
	}
	return nil
}

// ── print helpers ─────────────────────────────────────────────────────────────

func printRLTiers(p *Printer, tiers ...*adminv1.RateLimitTier) error {
	if p.Format != FormatTable {
		for _, t := range tiers {
			if err := p.Proto(t); err != nil {
				return err
			}
		}
		return nil
	}
	rows := make([]string, len(tiers))
	for i, t := range tiers {
		rows[i] = fmt.Sprintf("%s\t%d\t%d\t%d\t%.4f\t%s\t%s",
			t.GetName(),
			t.GetRpm(), t.GetRph(), t.GetRpd(),
			t.GetUsdPerDay(),
			onExceedLabel(t.GetOnExceed()),
			age(t.GetCreatedAt()),
		)
	}
	p.Table("NAME\tRPM\tRPH\tRPD\tUSD/DAY\tON_EXCEED\tAGE", rows)
	return nil
}

func printRLScopes(p *Printer, scopes ...*adminv1.RateLimitScope) error {
	if p.Format != FormatTable {
		for _, sc := range scopes {
			if err := p.Proto(sc); err != nil {
				return err
			}
		}
		return nil
	}
	rows := make([]string, len(scopes))
	for i, sc := range scopes {
		user := "-"
		if sc.User != nil && *sc.User != "" {
			user = *sc.User
		}
		rows[i] = fmt.Sprintf("%s\t%s\t%d entries", sc.GetWorkspaceId(), user, len(sc.GetEntries()))
	}
	p.Table("WORKSPACE-ID\tUSER\tENTRIES", rows)
	return nil
}

func printRLScopeDetail(p *Printer, sc *adminv1.RateLimitScope) error {
	return p.Proto(sc)
}

// ── shared parse/convert helpers ──────────────────────────────────────────────

// onExceedProtoFromString converts "reject"|"throttle"|"log_only" to the proto enum.
func onExceedProtoFromString(s string) adminv1.OnExceed {
	switch strings.ToLower(s) {
	case "throttle":
		return adminv1.OnExceed_ON_EXCEED_THROTTLE
	case "log_only", "log-only":
		return adminv1.OnExceed_ON_EXCEED_LOG_ONLY
	case "reject":
		return adminv1.OnExceed_ON_EXCEED_REJECT
	default:
		return adminv1.OnExceed_ON_EXCEED_UNSPECIFIED
	}
}

func onExceedLabel(oe adminv1.OnExceed) string {
	switch oe {
	case adminv1.OnExceed_ON_EXCEED_THROTTLE:
		return "throttle"
	case adminv1.OnExceed_ON_EXCEED_LOG_ONLY:
		return "log_only"
	case adminv1.OnExceed_ON_EXCEED_REJECT:
		return "reject"
	default:
		return "reject"
	}
}

// parseEntryJSONSlice unmarshals a slice of JSON strings into RateLimitPolicyEntry protos.
// Each JSON string must be a valid JSON object matching the proto field names (snake_case).
func parseEntryJSONSlice(jsonStrings []string) ([]*adminv1.RateLimitPolicyEntry, error) {
	entries := make([]*adminv1.RateLimitPolicyEntry, 0, len(jsonStrings))
	for i, s := range jsonStrings {
		var e adminv1.RateLimitPolicyEntry
		if err := protojson.Unmarshal([]byte(s), &e); err != nil {
			// Fall back to camelCase / raw JSON struct.
			return nil, fmt.Errorf("entry[%d]: invalid JSON: %w", i, err)
		}
		entries = append(entries, &e)
	}
	return entries, nil
}

// parseTierKVArgs parses key=value args like "rpm=100 on_exceed=throttle" into a tierFlags.
func parseTierKVArgs(kvs []string) tierFlags {
	var tf tierFlags
	tf.onExceed = "reject"
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "rpm":
			fmt.Sscanf(v, "%d", &tf.rpm)
		case "rph":
			fmt.Sscanf(v, "%d", &tf.rph)
		case "rpd":
			fmt.Sscanf(v, "%d", &tf.rpd)
		case "input_tpm", "input-tpm":
			fmt.Sscanf(v, "%d", &tf.inputTPM)
		case "input_tph", "input-tph":
			fmt.Sscanf(v, "%d", &tf.inputTPH)
		case "input_tpd", "input-tpd":
			fmt.Sscanf(v, "%d", &tf.inputTPD)
		case "output_tpm", "output-tpm":
			fmt.Sscanf(v, "%d", &tf.outputTPM)
		case "output_tph", "output-tph":
			fmt.Sscanf(v, "%d", &tf.outputTPH)
		case "output_tpd", "output-tpd":
			fmt.Sscanf(v, "%d", &tf.outputTPD)
		case "cache_read_tph", "cache-read-tph":
			fmt.Sscanf(v, "%d", &tf.cacheReadTPH)
		case "cache_read_tpd", "cache-read-tpd":
			fmt.Sscanf(v, "%d", &tf.cacheReadTPD)
		case "cache_write_tph", "cache-write-tph":
			fmt.Sscanf(v, "%d", &tf.cacheWriteTPH)
		case "cache_write_tpd", "cache-write-tpd":
			fmt.Sscanf(v, "%d", &tf.cacheWriteTPD)
		case "usd_per_minute", "usd-per-minute":
			fmt.Sscanf(v, "%f", &tf.usdPerMinute)
		case "usd_per_hour", "usd-per-hour":
			fmt.Sscanf(v, "%f", &tf.usdPerHour)
		case "usd_per_day", "usd-per-day":
			fmt.Sscanf(v, "%f", &tf.usdPerDay)
		case "on_exceed", "on-exceed":
			tf.onExceed = v
		}
	}
	return tf
}

// kvGet returns the first value for key in key=value token list.
func kvGet(tokens []string, key string) string {
	for _, t := range tokens {
		k, v, ok := strings.Cut(t, "=")
		if ok && k == key {
			return v
		}
	}
	return ""
}

// kvGetAll returns all values for key in key=value token list.
func kvGetAll(tokens []string, key string) []string {
	var vals []string
	for _, t := range tokens {
		k, v, ok := strings.Cut(t, "=")
		if ok && k == key {
			vals = append(vals, v)
		}
	}
	return vals
}
