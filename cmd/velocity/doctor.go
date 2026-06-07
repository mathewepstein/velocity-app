package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mathewepstein/velocity/internal/doctor"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func doctorCmd() *cobra.Command {
	var noColor bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Validate config, credentials, and cache integrity",
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			useColor := !noColor && isTerminal(w)
			summary := doctor.Run(time.Now())
			renderSummary(w, summary, useColor)
			if summary.Fail > 0 {
				// Exit non-zero so scripts (e.g., CI health checks) can detect.
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noColor, "no-color", false, "Disable ANSI color output")
	return cmd
}

// renderSummary prints the summary as a human-readable report. Separated from
// doctor.Run so the package stays pure and testable.
func renderSummary(w io.Writer, s *doctor.Summary, useColor bool) {
	fmt.Fprintln(w, "velocity doctor — checking your setup")
	fmt.Fprintln(w)
	for _, c := range s.Checks {
		prefix := statusPrefix(c.Status, useColor)
		fmt.Fprintf(w, "  %s %-30s %s\n", prefix, c.Name, c.Message)
		if c.Details != "" {
			// Indent every line of Details to match the message column.
			for _, line := range splitLines(c.Details) {
				fmt.Fprintf(w, "      %s\n", line)
			}
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %d check(s) · %d ok · %d warn · %d fail\n",
		len(s.Checks), s.OK, s.Warn, s.Fail)
	if s.Fail > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  Fix the failures above, then re-run `velocity doctor`.")
	}
}

func statusPrefix(st doctor.Status, useColor bool) string {
	switch st {
	case doctor.StatusOK:
		if useColor {
			return "\033[32m[ OK ]\033[0m"
		}
		return "[ OK ]"
	case doctor.StatusWarn:
		if useColor {
			return "\033[33m[WARN]\033[0m"
		}
		return "[WARN]"
	case doctor.StatusFail:
		if useColor {
			return "\033[31m[FAIL]\033[0m"
		}
		return "[FAIL]"
	}
	return "[?   ]"
}

// isTerminal probes whether w is a TTY. Honors the widely-supported NO_COLOR
// env var. Non-TTY (e.g., piped into less) never gets color.
func isTerminal(w io.Writer) bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

func splitLines(s string) []string {
	// strings.Split would produce a trailing empty on "foo\n"; we want the
	// message split on embedded newlines, no trailing whitespace rows.
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
