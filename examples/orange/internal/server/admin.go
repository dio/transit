package server

import "github.com/spf13/cobra"

func newAdminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Management plane administration",
		Long: `Admin commands for managing orgs, projects, workspaces, users, and secrets.

Run 'orange admin <command> --help' for command-specific help.`,
		SilenceUsage: true,
	}
	cmd.AddCommand(newOrgCmd())
	cmd.AddCommand(newProjectCmd())
	cmd.AddCommand(newWorkspaceCmd())
	cmd.AddCommand(newUserCmd())
	cmd.AddCommand(newMemberCmd())
	cmd.AddCommand(newSecretCmd())
	cmd.AddCommand(newAPIKeyCmd())
	cmd.AddCommand(newEgressCmd())
	cmd.AddCommand(newConfigCmd())
	cmd.AddCommand(newRLTierCmd())
	cmd.AddCommand(newRLScopeCmd())
	cmd.AddCommand(newPolicyAdminCmd())
	cmd.AddCommand(newKeyEntryCmd())
	cmd.AddCommand(newKeyEntryTokenCmd())
	cmd.AddCommand(newKeyEntrySecretCmd())
	cmd.AddCommand(newKeyEntryRoutingCmd())
	cmd.AddCommand(newListCmd())
	cmd.AddCommand(newReplCmd())
	return cmd
}
