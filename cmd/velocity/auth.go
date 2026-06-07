package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/secrets"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func authCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage API tokens stored in the OS keychain",
	}
	cmd.AddCommand(authSetCmd(), authShowCmd(), authDeleteCmd())
	return cmd
}

func authSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <service>",
		Short: "Prompt for a token and store it in the keychain (service: jira | github)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := strings.ToLower(args[0])
			if !secrets.IsKnownService(service) {
				return fmt.Errorf("unknown service %q (expected one of: %s)", service, strings.Join(secrets.KnownServices, ", "))
			}

			token, err := readToken(cmd.InOrStdin(), cmd.ErrOrStderr(), service)
			if err != nil {
				return err
			}

			if err := secrets.Set(config.DefaultProfile, service, token); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Stored %s token for profile %q.\n", service, config.DefaultProfile)
			return nil
		},
	}
}

func authShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "List services with a stored token (never prints the token itself)",
		RunE: func(cmd *cobra.Command, args []string) error {
			profile := config.DefaultProfile
			fmt.Fprintf(cmd.OutOrStdout(), "Profile: %s\n", profile)
			for _, service := range secrets.KnownServices {
				has, err := secrets.Has(profile, service)
				if err != nil {
					return fmt.Errorf("check %s: %w", service, err)
				}
				status := "not set"
				if has {
					status = "set"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %-8s %s  (%s)\n", service, status, secrets.Key(profile, service))
			}
			return nil
		},
	}
}

func authDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <service>",
		Short: "Remove a stored token from the keychain",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := strings.ToLower(args[0])
			if !secrets.IsKnownService(service) {
				return fmt.Errorf("unknown service %q (expected one of: %s)", service, strings.Join(secrets.KnownServices, ", "))
			}
			err := secrets.Delete(config.DefaultProfile, service)
			if errors.Is(err, secrets.ErrNotFound) {
				fmt.Fprintf(cmd.OutOrStdout(), "No %s token stored for profile %q.\n", service, config.DefaultProfile)
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s token for profile %q.\n", service, config.DefaultProfile)
			return nil
		},
	}
}

// readToken reads a token from stdin. If stdin is a terminal, prompts with
// echo disabled. If stdin is piped (e.g. from a secrets manager), reads a
// line raw with no prompt. Empty input is rejected.
func readToken(in io.Reader, errOut io.Writer, service string) (string, error) {
	// When stdin is a terminal, use no-echo read. Detecting a terminal
	// requires an *os.File; cmd.InOrStdin() is an io.Reader, so we check
	// os.Stdin directly — the only case where we need interactive input.
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprintf(errOut, "Enter %s token (input hidden): ", service)
		tok, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(errOut) // newline after hidden input
		if err != nil {
			return "", fmt.Errorf("read token: %w", err)
		}
		trimmed := strings.TrimSpace(string(tok))
		if trimmed == "" {
			return "", fmt.Errorf("token is empty")
		}
		return trimmed, nil
	}

	// Piped / non-interactive: read one line from stdin.
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 8*1024), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", fmt.Errorf("read token: %w", err)
		}
		return "", fmt.Errorf("no token on stdin")
	}
	trimmed := strings.TrimSpace(scanner.Text())
	if trimmed == "" {
		return "", fmt.Errorf("token is empty")
	}
	return trimmed, nil
}
