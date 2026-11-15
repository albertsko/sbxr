package cli

import (
	"encoding/json"
	"errors"
	"io"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/albertsko/sbxr/internal/preflight"
)

func (a *app) doctorCommand() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the machine and print fix commands",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := cacheDir()
			if err != nil {
				return failure(cmd, err)
			}

			report := preflight.Run(cmd.Context(), a.backend.Checks(dir))

			if asJSON {
				err = writeJSON(cmd.OutOrStdout(), report)
			} else {
				err = writeReport(cmd.OutOrStdout(), report)
			}

			if err != nil {
				return failure(cmd, err)
			}

			if !report.OK {
				return &exitError{code: exitFailure, err: nil}
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Print the report as JSON")
	cmd.AddCommand(&cobra.Command{
		Use:   "apparmor-profile",
		Short: "Print the AppArmor profile that allows sbxr to use user namespaces",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := io.WriteString(cmd.OutOrStdout(), preflight.AppArmorProfile)

			return err //nolint:wrapcheck // A failed write needs no context.
		},
	})

	return cmd
}

func writeJSON(w io.Writer, report preflight.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(report) //nolint:wrapcheck // A failed write needs no context.
}

// writeReport prints one line per check, the fix of each failed check, and a combined fix script.
func writeReport(w io.Writer, report preflight.Report) error {
	pass := lipgloss.NewStyle().Foreground(lipgloss.Green).Render("pass")
	fail := lipgloss.NewStyle().Foreground(lipgloss.Red).Render("fail")

	var errs []error

	printf := func(format string, args ...any) {
		_, err := lipgloss.Fprintf(w, format, args...)
		errs = append(errs, err)
	}

	for _, res := range report.Checks {
		if res.Status == preflight.Pass {
			printf("%s  %-11s  %s\n", pass, res.ID, res.Detail)

			continue
		}

		printf("%s  %-11s  %s\n      fix: %s\n", fail, res.ID, res.Detail, res.Fix)
	}

	if failed := report.Failures(); len(failed) > 1 {
		printf("\nFix all:\n")

		for _, res := range failed {
			printf("%s\n", res.Fix)
		}
	}

	return errors.Join(errs...)
}

func (a *app) pruneCommand() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove cached images",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return failure(cmd, errors.New("prune: not implemented yet"))
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Do not ask for confirmation")

	return cmd
}
