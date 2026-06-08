package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/chzyer/readline"

	adminv1 "github.com/dio/transit/examples/orange/api/orange/config/admin/v1"
	configconnect "github.com/dio/transit/examples/orange/api/orange/config/admin/v1/adminv1connect"
	keyentryv1 "github.com/dio/transit/examples/orange/api/orange/keyentry/admin/v1"
	keyentryconnect "github.com/dio/transit/examples/orange/api/orange/keyentry/admin/v1/adminv1connect"
	secretv1 "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1"
	secretconnect "github.com/dio/transit/examples/orange/api/orange/secret/admin/v1/adminv1connect"
	workspacev1 "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1"
	workspaceconnect "github.com/dio/transit/examples/orange/api/orange/workspace/admin/v1/adminv1connect"
)

// userReplState holds the active workspace context for the user-facing REPL.
type userReplState struct {
	rc     *RunCtx
	wsID   string
	wsName string
}

func (s *userReplState) prompt() string {
	if s.wsName != "" {
		return fmt.Sprintf("orange [%s]> ", s.wsName)
	}
	if s.wsID != "" {
		return fmt.Sprintf("orange [%s]> ", shortID(s.wsID))
	}
	return "orange> "
}

// runUserREPL is the entry point for the user-facing interactive REPL.
// seedArgs are key=value pairs (currently only ws=<id> is supported).
func runUserREPL(rc *RunCtx, seedArgs []string) error {
	histFile := os.ExpandEnv("$HOME/.orange/user_repl_history")
	_ = os.MkdirAll(os.ExpandEnv("$HOME/.orange"), 0o700)

	s := &userReplState{rc: rc}

	for _, arg := range seedArgs {
		k, v, ok := strings.Cut(arg, "=")
		if !ok || v == "" {
			fmt.Fprintf(os.Stderr, "warning: ignoring seed arg %q — expected key=value\n", arg)
			continue
		}
		if k != "ws" && k != "workspace" {
			fmt.Fprintf(os.Stderr, "warning: unknown seed key %q — valid keys: ws\n", k)
			continue
		}
		if err := s.cmdUseWS(v); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not seed ws=%s: %v\n", v, err)
		}
	}

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          s.prompt(),
		HistoryFile:     histFile,
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		return err
	}
	defer rl.Close()

	fmt.Println("orange REPL  •  type 'help' for commands, 'exit' or Ctrl+D to quit")

	for {
		rl.SetPrompt(s.prompt())
		line, err := rl.Readline()
		if errors.Is(err, readline.ErrInterrupt) {
			if line == "" {
				break
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if dispatchErr := s.dispatch(line); dispatchErr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", dispatchErr)
		}
	}
	return nil
}

func (s *userReplState) dispatch(line string) error {
	toks := tokenize(line)
	if len(toks) == 0 {
		return nil
	}

	switch toks[0] {
	case "exit", "quit":
		os.Exit(0)

	case "help", "?":
		printUserReplHelp()

	case "context", "ctx":
		s.printContext()

	case "use":
		if len(toks) < 3 {
			return fmt.Errorf("usage: use ws <id>")
		}
		if toks[1] != "ws" && toks[1] != "workspace" {
			return fmt.Errorf("unknown resource %q — try: ws", toks[1])
		}
		return s.cmdUseWS(toks[2])

	case "unset":
		if len(toks) < 2 {
			return fmt.Errorf("usage: unset ws")
		}
		if toks[1] != "ws" && toks[1] != "workspace" {
			return fmt.Errorf("unknown resource %q — try: ws", toks[1])
		}
		s.wsID, s.wsName = "", ""

	case "ws", "workspace":
		return s.cmdWS(toks[1:])

	case "token", "tok":
		return s.cmdToken(toks[1:])

	case "secret", "sec":
		return s.cmdSecret(toks[1:])

	case "whoami":
		return s.cmdWhoami()

	case "rl-tier", "rlt":
		return s.cmdUserRLTier(toks[1:])

	case "rl-scope", "rls":
		return s.cmdUserRLScope(toks[1:])

	case "keyentry", "ke":
		return s.cmdUserKeyEntry(toks[1:])

	case "keyentry-token", "ket":
		return s.cmdUserKeyEntryToken(toks[1:])

	case "keyentry-secret", "kes":
		return s.cmdUserKeyEntrySecret(toks[1:])

	case "keyentry-routing", "ker":
		return s.cmdUserKeyEntryRouting(toks[1:])

	default:
		return fmt.Errorf("unknown command %q — type 'help' for a list", toks[0])
	}
	return nil
}

// ── navigation ────────────────────────────────────────────────────────────────

func (s *userReplState) cmdUseWS(id string) error {
	ctx := context.Background()
	client := workspaceconnect.NewWorkspaceAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)
	resp, err := client.GetWorkspace(ctx, connect.NewRequest(&workspacev1.GetWorkspaceRequest{WorkspaceId: id}))
	if err != nil {
		return err
	}
	w := resp.Msg.GetWorkspace()
	s.wsID, s.wsName = w.GetWorkspaceId(), w.GetName()
	fmt.Printf("workspace: %s (%s)\n", s.wsName, s.wsID)
	return nil
}

func (s *userReplState) printContext() {
	if s.wsID == "" {
		fmt.Println("no workspace set — use 'use ws <id>' to start")
		return
	}
	fmt.Printf("workspace: %s (%s)\n", s.wsName, s.wsID)
}

// ── ws ────────────────────────────────────────────────────────────────────────

func (s *userReplState) cmdWS(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := workspaceconnect.NewWorkspaceAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "get":
		id := s.wsID
		if len(args) > 1 {
			id = args[1]
		}
		if id == "" {
			return fmt.Errorf("no workspace in context — provide an id or run 'use ws <id>'")
		}
		resp, err := client.GetWorkspace(ctx, connect.NewRequest(&workspacev1.GetWorkspaceRequest{WorkspaceId: id}))
		if err != nil {
			return err
		}
		return printWorkspaces(s.rc.Printer, resp.Msg.GetWorkspace())

	default:
		return fmt.Errorf("unknown ws subcommand %q — try: get [id]", sub)
	}
}

// ── token ─────────────────────────────────────────────────────────────────────

func (s *userReplState) cmdToken(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()

	switch sub {
	case "create":
		// token create <name> [ttl=<seconds>]
		if len(args) < 2 {
			return fmt.Errorf("usage: token create <name> [ttl=<seconds>]")
		}
		name := args[1]
		var ttl int64
		for _, a := range args[2:] {
			k, v, ok := strings.Cut(a, "=")
			if !ok {
				continue
			}
			if k == "ttl" {
				if _, err := fmt.Sscanf(v, "%d", &ttl); err != nil {
					return fmt.Errorf("invalid ttl %q: must be an integer (seconds)", v)
				}
			}
		}

		client := keyentryconnect.NewKeyEntryAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)
		req := &keyentryv1.IssueNamedTokenRequest{
			Name:       name,
			TtlSeconds: ttl,
		}
		resp, err := client.IssueNamedToken(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		return printToken(s.rc.Printer, resp.Msg.GetToken(), resp.Msg.GetMetadata())

	default:
		return fmt.Errorf("unknown token subcommand %q — try: create <name> [ttl=<seconds>]", sub)
	}
}

// ── secret ────────────────────────────────────────────────────────────────────

// cmdSecret provides user-scoped secret read access.
//
// Requires ws context for the short forms. Realm is constructed as:
//
//	ws/<wsID>/<purpose>    when ws context is set and purpose is given
//
// Usage:
//
//	secret ls [<purpose>]          list secrets in ws realm (requires ws context)
//	secret get <purpose> <name>    resolve secret (requires ws context)
func (s *userReplState) cmdSecret(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	client := secretconnect.NewSecretAdminServiceClient(s.rc.HTTPClient, s.rc.ServerURL, s.rc.ConnectOpts...)

	switch sub {
	case "ls", "list":
		if s.wsID == "" {
			return fmt.Errorf("no workspace in context — run 'use ws <id>' first")
		}
		realm := "ws/" + s.wsID
		if len(args) > 1 {
			realm = "ws/" + s.wsID + "/" + args[1]
		}
		resp, err := client.ListSecrets(ctx, connect.NewRequest(&secretv1.ListSecretsRequest{Realm: realm}))
		if err != nil {
			return err
		}
		return printSecretSummaries(s.rc.Printer, resp.Msg.GetSecrets()...)

	case "get":
		if len(args) < 3 {
			return fmt.Errorf("usage: secret get <purpose> <name>")
		}
		if s.wsID == "" {
			return fmt.Errorf("no workspace in context — run 'use ws <id>' first")
		}
		realm := "ws/" + s.wsID + "/" + args[1]
		name := args[2]
		resp, err := client.ResolveVersion(ctx, connect.NewRequest(&secretv1.ResolveVersionRequest{
			Realm:    realm,
			SecretId: name,
		}))
		if err != nil {
			return err
		}
		fmt.Println(string(resp.Msg.GetVersion().GetMaterial()))
		return nil

	default:
		return fmt.Errorf("unknown secret subcommand %q — try: ls [purpose], get <purpose> <name>", sub)
	}
}

// ── whoami ────────────────────────────────────────────────────────────────────

func (s *userReplState) cmdWhoami() error {
	cfg, err := LoadConfig()
	if err != nil {
		// Fall back to showing just the server URL.
		fmt.Printf("server: %s\n", s.rc.ServerURL)
		return nil
	}
	org := cfg.ActiveOrg
	fmt.Printf("server: %s\n", s.rc.ServerURL)
	if org != "" {
		fmt.Printf("org:    %s\n", org)
		if entry, ok := cfg.Orgs[org]; ok {
			fmt.Printf("user:   %s\n", entry.ActiveUser)
		}
	}
	return nil
}

// ── rl-tier (read-only for users) ─────────────────────────────────────────────

// cmdUserRLTier exposes read-only tier listing so users can see available tiers.
//
//	rl-tier ls [<ws-id>]
//	rl-tier get <name> [<ws-id>]
func (s *userReplState) cmdUserRLTier(args []string) error {
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

	default:
		return fmt.Errorf("unknown rl-tier subcommand %q — try: ls, get", sub)
	}
}

// ── rl-scope (user self-service) ──────────────────────────────────────────────

// cmdUserRLScope lets the user author their own rate-limit scope within a workspace.
// Requires the API key to carry rl-policy:write[ws-id/user-id] scope.
//
//	rl-scope get user=<id>
//	rl-scope set user=<id> entry='<json>' [entry='<json>' ...]
//	rl-scope ls [<ws-id>]
//	rl-scope delete user=<id>
func (s *userReplState) cmdUserRLScope(args []string) error {
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
				"  example: rl-scope set user=<id> entry='{\"models\":[\"*\"],\"rpm\":100}'")
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

// ── keyentry (user self-service) ──────────────────────────────────────────────

// cmdUserKeyEntry lets the user manage their own token slots.
// Requires keyentry:write[ws] scope.
//
//	keyentry ls [<ws-id>]
//	keyentry get <key-entry-id>
//	keyentry create <name> user=<id> [desc=<text>]
//	keyentry update <key-entry-id> [desc=<text>]
//	keyentry delete <key-entry-id>
func (s *userReplState) cmdUserKeyEntry(args []string) error {
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
			return fmt.Errorf("usage: keyentry create <name> user=<id> [desc=<text>]")
		}
		name := args[1]
		wsID := s.wsID
		if wsID == "" {
			return fmt.Errorf("no workspace in context — run 'use ws <id>' first")
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

// ── keyentry-token (user self-service) ───────────────────────────────────────────────

// cmdUserKeyEntryToken lets the user manage PASETO tokens on their token slots.
// Requires keyentry:write[ws] scope.
//
//	keyentry-token ls <key-entry-id>
//	keyentry-token get <token-id>
//	keyentry-token issue <key-entry-id> [ttl=<seconds>]
//	keyentry-token revoke <token-id>
func (s *userReplState) cmdUserKeyEntryToken(args []string) error {
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
		resp, err := client.IssueToken(ctx, connect.NewRequest(&keyentryv1.IssueTokenRequest{
			KeyEntryId: args[1],
			TtlSeconds: ttl,
		}))
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

// ── keyentry-secret (user self-service) ──────────────────────────────────────────────

// cmdUserKeyEntrySecret lets the user manage upstream API secrets on their token slots.
// Requires keyentry:write[ws] scope.
//
//	keyentry-secret ls <key-entry-id>
//	keyentry-secret get <key-secret-id>
//	keyentry-secret create <key-entry-id> target=<upstream> [desc=<text>]   (prompts for value)
//	keyentry-secret rotate <key-secret-id>                                   (prompts for value)
//	keyentry-secret delete <key-secret-id>
func (s *userReplState) cmdUserKeyEntrySecret(args []string) error {
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

// readHiddenInput prompts the user and reads a line with terminal echo disabled.
func (s *userReplState) readHiddenInput(prompt string) ([]byte, error) {
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

// ── help ──────────────────────────────────────────────────────────────────────

func printUserReplHelp() {
	fmt.Print(`
Start with workspace context:
  orange --repl ws=<id>

Navigation:
  use ws <id>       set workspace context
  unset ws          clear workspace context
  context / ctx     show current context

Workspace:
  ws get [id]       get workspace info (defaults to current)

Token  (requires token:issue[ws] scope):
  token create <name> [ttl=<seconds>]   issue a named PASETO token (shown once)

Secret  (requires secret:read[ws] scope):
  secret ls [<purpose>]          list secrets in workspace (e.g. purpose = default)
  secret get <purpose> <name>    resolve and print secret material

Rate-limit tiers  (requires rl-policy:write[ws] scope; alias: rlt):
  rl-tier ls [<ws-id>]           list tiers available in workspace
  rl-tier get <name> [<ws-id>]   show tier details

Rate-limit scope  (requires rl-policy:write[ws/user] scope; alias: rls):
  rl-scope ls [<ws-id>]                     list scopes visible to your key
  rl-scope get user=<id>                    get your user scope
  rl-scope set user=<id> entry='<json>' [entry='<json>' ...]
                                            set your user scope entries (full replace)
  rl-scope delete user=<id>                 remove your user scope

  Entry JSON: {"models":["*"],"rpm":100,"on_exceed":"throttle"}
  Single-quote JSON to handle spaces: entry='{"models": ["*"], "rpm": 100}'

Key entries / token slots  (requires keyentry:write[ws] scope; alias: ke):
  keyentry ls [<ws-id>]                    list your token slots in workspace
  keyentry get <key-entry-id>              get a token slot
  keyentry create <name> user=<id> [desc=<text>]
                                           create a token slot for yourself
  keyentry update <key-entry-id> [desc=<text>]
                                           update description
  keyentry delete <key-entry-id>           delete the slot

PASETO tokens  (requires keyentry:write[ws] scope; alias: ket):
  keyentry-token ls <key-entry-id>               list tokens issued from a slot
  keyentry-token get <token-id>                  get token record
  keyentry-token issue <key-entry-id> [ttl=<s>]  issue an anonymous PASETO token (shown once)
  keyentry-token revoke <token-id>               revoke a token

Key secrets  (requires keyentry:write[ws] scope; alias: kes):
  keyentry-secret ls <key-entry-id>                      list key secrets for a slot
  keyentry-secret get <key-secret-id>                    get a key secret record
  keyentry-secret create <key-entry-id> target=<upstream> [desc=<text>]
                                                  create (prompts for value, hidden)
  keyentry-secret rotate <key-secret-id>                 rotate (prompts for new value, hidden)
  keyentry-secret delete <key-secret-id>                 delete

Routing overrides  (requires keyentry:write[ws] scope; alias: ker):
  keyentry-routing targets [<ws-id>]               list LLM providers available as routing targets
  keyentry-routing models <provider> [<ws-id>]     list backend model names for a provider
  keyentry-routing validate file=<path> [ws=<id>]  parse & preview rules locally (no server write)
  keyentry-routing get <key-entry-id>              show routing overrides
  keyentry-routing set <key-entry-id> file=<path>  replace from YAML (or .json)
  keyentry-routing set <key-entry-id> json='{"rules":[{"model":"...","target":"..."}]}'
  keyentry-routing delete <key-entry-id>           clear all overrides (reverts to workspace default)

  Rule fields: model (client-facing model ID), target (llm.providers key — run 'targets' to list valid values)
               backend_model (optional): overrides the model name sent to the upstream
  Advanced:    chain: [{target:A}, {target:B}]  ordered fallback (by provider or by backend_model)
               split: [{weight:80,target:A}, {weight:20,target:A,backend_model:B}]  A/B by provider or model

Other:
  whoami            show active server / org / user from config
  help / ?          show this help
  exit / quit / Ctrl+D  exit REPL

ORANGE_API_KEY=<key> orange --repl ws=<ws-id>

`)
}
