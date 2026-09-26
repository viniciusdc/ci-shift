package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/viniciusdc/ci-shift/schema"
)

func newSchemasCmd(stdout, stderr io.Writer) *cobra.Command {
	status := func(cmd *cobra.Command, args []string) error {
		docs, err := schema.Status(schema.Options{})
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "PROVIDER\tSOURCE\tFINGERPRINT\tUPDATED")
		for _, d := range docs {
			updated := "-"
			if !d.UpdatedAt.IsZero() {
				updated = d.UpdatedAt.Format("2006-01-02 15:04")
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", d.Provider, d.Origin, d.Fingerprint(), updated)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\ncache: %s\n", schema.CacheDir(schema.Options{}))
		return nil
	}
	cmd := &cobra.Command{
		Use:   "schemas",
		Short: "Show or update the JSON schemas used by 'cishift check'",
		Long: `cishift ships a snapshot of every provider's schema, so checks work offline
and give the same result everywhere. 'cishift schemas update' downloads the
current schemas into your cache; from then on they are used instead of the
built-in ones. 'cishift schemas reset' goes back to the built-in snapshots.`,
		Args: cobra.NoArgs,
		RunE: status,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "status",
			Short: "Show which schema each provider uses",
			Args:  cobra.NoArgs,
			RunE:  status,
		},
		&cobra.Command{
			Use:   "update",
			Short: "Download the current schemas into the cache",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				docs, errs := schema.Update(schema.Options{})
				for _, d := range docs {
					fmt.Fprintf(stdout, "%s: updated (%s)\n", d.Provider, d.Fingerprint())
				}
				for provider, err := range errs {
					fmt.Fprintf(stderr, "%s: %v\n", provider, err)
				}
				if len(errs) > 0 {
					return ExitError{Code: 1}
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "reset",
			Short: "Remove downloaded schemas and use the built-in snapshots",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := schema.Reset(schema.Options{}); err != nil {
					return err
				}
				fmt.Fprintln(stdout, "using the built-in schemas")
				return nil
			},
		},
	)
	return cmd
}
