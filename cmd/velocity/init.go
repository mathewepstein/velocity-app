package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/initflow"
	"github.com/spf13/cobra"
)

func initCmd() *cobra.Command {
	var template bool
	var force bool
	var noEnv bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Interactive first-run setup (or --template to drop a commented config)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if template {
				return runInitTemplate(cmd, force)
			}
			return runInitInteractive(cmd, force, noEnv)
		},
	}
	cmd.Flags().BoolVar(&template, "template", false, "Write a commented config template instead of prompting")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite an existing config file")
	cmd.Flags().BoolVar(&noEnv, "no-env", false, "Ignore ATLASSIAN_EMAIL / ATLASSIAN_API_TOKEN / GH_TOKEN / GITHUB_ORG and prompt for everything")
	return cmd
}

// runInitTemplate writes the commented TOML template to the resolved config
// path. Refuses to overwrite unless --force. Creates parent dirs with 0o700.
func runInitTemplate(cmd *cobra.Command, force bool) error {
	path, err := config.Path()
	if err != nil {
		return err
	}

	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("config already exists at %s (re-run with --force to overwrite)", path)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	if err := config.EnsureDir(path); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	if err := os.WriteFile(path, []byte(config.Template()), 0o600); err != nil {
		return fmt.Errorf("write template: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Wrote config template to %s\n", path)
	fmt.Fprintln(cmd.OutOrStdout(), "Next:")
	fmt.Fprintln(cmd.OutOrStdout(), "  1. Edit the file to fill in your Atlassian URL, email, project keys, GitHub org, etc.")
	fmt.Fprintln(cmd.OutOrStdout(), "  2. Store your API tokens:")
	fmt.Fprintln(cmd.OutOrStdout(), "       velocity auth set jira")
	fmt.Fprintln(cmd.OutOrStdout(), "       velocity auth set github")
	return nil
}

// runInitInteractive walks the user through the full guided setup.
func runInitInteractive(cmd *cobra.Command, force, noEnv bool) error {
	path, err := config.Path()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("config already exists at %s (re-run with --force to overwrite, or --template to hand-edit)", path)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	return initflow.Run(cmd.Context(), initflow.Options{Out: cmd.OutOrStdout(), NoEnv: noEnv})
}
