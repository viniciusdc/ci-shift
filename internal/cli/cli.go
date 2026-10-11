// Package cli implements the cishift command line.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/viniciusdc/ci-shift/convert"
	"github.com/viniciusdc/ci-shift/internal/buildinfo"
	"github.com/viniciusdc/ci-shift/internal/lockfile"
	"github.com/viniciusdc/ci-shift/ir"
	"github.com/viniciusdc/ci-shift/schema"
)

// ExitError carries a process exit code without printing a Go error.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// Execute runs the CLI with os.Args and returns the process exit code.
func Execute() int {
	return Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

// Run executes the CLI with explicit arguments and streams (used by tests).
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	root := newRoot(stdin, stdout, stderr)
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.Execute()
	if err == nil {
		return 0
	}
	var ee ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	fmt.Fprintln(stderr, "error:", err)
	return 2
}

func newRoot(stdin io.Reader, stdout, stderr io.Writer) *cobra.Command {
	var opts convertOpts
	root := &cobra.Command{
		Use:   "cishift <file|-> --to <provider>",
		Short: "Shift CI/CD pipelines between providers",
		Long: `ci-shift translates CI/CD configurations between GitHub Actions, GitLab CI/CD,
CircleCI, Azure Pipelines and Bitbucket Pipelines through a shared pipeline
model, and checks them against each provider's JSON schema.`,
		Example: `  cishift .gitlab-ci.yml --to github -o .github/workflows/ci.yml
  cishift .circleci/config.yml --to azure --check --strict
  cat bitbucket-pipelines.yml | cishift - --from bitbucket --to gitlab
  cishift check .github/workflows/*.yml`,
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       buildinfo.Version(),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return runConvert(args[0], opts, stdin, stdout, stderr)
		},
	}
	opts.bind(root)

	// "convert" stays available for scripts that prefer an explicit verb.
	var aliasOpts convertOpts
	convertCmd := &cobra.Command{
		Use:    "convert <file|-> --to <provider>",
		Short:  "Convert a pipeline (same as running cishift with a file)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConvert(args[0], aliasOpts, stdin, stdout, stderr)
		},
	}
	aliasOpts.bind(convertCmd)

	root.AddCommand(
		convertCmd,
		newCheckCmd(stdin, stdout),
		newDetectCmd(stdin, stdout),
		newProvidersCmd(stdout),
		newVerifyCmd(stdout),
		newSyncCmd(stdout, stderr),
		newSchemasCmd(stdout, stderr),
		newVersionCmd(stdout),
	)
	return root
}

func readInput(path string, stdin io.Reader) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(stdin)
	}
	return os.ReadFile(path)
}

func resolveProvider(name, path string, data []byte) (ir.Provider, error) {
	if name != "" {
		return convert.Get(name)
	}
	return convert.Detect(path, data)
}

// convertOpts are the flags of a conversion.
type convertOpts struct {
	from, to, output, lock                        string
	noHeader, strict, check, quiet, noLock, force bool
}

func (o *convertOpts) bind(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVarP(&o.from, "from", "f", "", "source provider (detected from the file when omitted)")
	f.StringVarP(&o.to, "to", "t", "", "target provider: "+strings.Join(convert.Names(), ", "))
	f.StringVarP(&o.output, "output", "o", "", "output file (default stdout)")
	f.BoolVar(&o.noHeader, "no-header", false, "omit the review comment header")
	f.BoolVar(&o.strict, "strict", false, "exit 1 when the conversion produced warnings")
	f.BoolVar(&o.check, "check", false, "check the output against the target provider's schema")
	f.BoolVarP(&o.quiet, "quiet", "q", false, "do not print diagnostics")
	f.StringVar(&o.lock, "lock", "", "lockfile to record the output in (default nearest "+lockfile.Name+")")
	f.BoolVar(&o.noLock, "no-lock", false, "do not record the output in the lockfile")
	f.BoolVar(&o.force, "force", false, "overwrite an output that was edited by hand")
}

func runConvert(path string, o convertOpts, stdin io.Reader, stdout, stderr io.Writer) error {
	if o.to == "" {
		return fmt.Errorf("missing --to <provider> (one of %s)", strings.Join(convert.Names(), ", "))
	}
	data, err := readInput(path, stdin)
	if err != nil {
		return err
	}
	src, err := resolveProvider(o.from, path, data)
	if err != nil {
		return err
	}
	dst, err := convert.Get(o.to)
	if err != nil {
		return err
	}
	toFile := o.output != "" && o.output != "-"

	// Files written to disk are recorded in the lockfile, keyed by their
	// lockfile-relative path; the header then names the source the same way
	// so `cishift sync` reproduces it byte for byte from anywhere.
	var lf *lockfile.File
	var entry lockfile.Entry
	sourceName := displayName(path)
	if toFile && !o.noLock {
		if path == "-" {
			if !o.quiet {
				fmt.Fprintln(stderr, "info: output converted from stdin is not recorded in the lockfile")
			}
		} else {
			if lf, err = openLock(o.lock); err != nil {
				return err
			}
			if entry.Output, err = lf.Rel(o.output); err != nil {
				return err
			}
			if entry.Source, err = lf.Rel(path); err != nil {
				return err
			}
			sourceName = entry.Source
			if err := guardHandEdits(lf, entry.Output, o.force); err != nil {
				return err
			}
		}
	}

	res, err := convert.Convert(data, src, dst, convert.Options{
		Header:     !o.noHeader,
		SourceName: sourceName,
	})
	if err != nil {
		return err
	}
	if !o.quiet {
		for _, d := range res.Diagnostics {
			fmt.Fprintln(stderr, d)
		}
	}
	if !toFile {
		if _, err := stdout.Write(res.Output); err != nil {
			return err
		}
	} else {
		if err := writeFile(o.output, res.Output); err != nil {
			return err
		}
		if !o.quiet {
			fmt.Fprintf(stderr, "wrote %s (%s -> %s)\n", o.output, src.Title(), dst.Title())
		}
		if lf != nil {
			entry.From, entry.To, entry.NoHeader = src.Name(), dst.Name(), o.noHeader
			if err := record(lf, entry, data, res.Output); err != nil {
				return err
			}
			if !o.quiet {
				fmt.Fprintf(stderr, "recorded in %s\n", displayPath(lf.Path()))
			}
		}
	}
	failed := false
	if o.check {
		issues, err := schema.Validate(dst.Name(), res.Output, schema.Options{})
		if err != nil {
			return err
		}
		for _, i := range issues {
			fmt.Fprintf(stderr, "schema: %s\n", i)
		}
		failed = len(issues) > 0
	}
	if o.strict {
		for _, d := range res.Diagnostics {
			if d.Severity == ir.SeverityWarning {
				failed = true
				break
			}
		}
	}
	if failed {
		return ExitError{Code: 1}
	}
	return nil
}

func writeFile(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

func displayName(path string) string {
	if path == "-" {
		return "stdin"
	}
	return filepath.ToSlash(path)
}

func newCheckCmd(stdin io.Reader, stdout io.Writer) *cobra.Command {
	var provider string
	var latest, embedded bool
	cmd := &cobra.Command{
		Use:     "check <file|->...",
		Aliases: []string{"validate"},
		Short:   "Check pipeline files against their provider's JSON schema",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			failed := false
			for _, path := range args {
				data, err := readInput(path, stdin)
				if err != nil {
					return err
				}
				p, err := resolveProvider(provider, path, data)
				if err != nil {
					return err
				}
				issues, err := schema.Validate(p.Name(), data, schema.Options{Latest: latest, Embedded: embedded})
				if err != nil {
					return err
				}
				if len(issues) == 0 {
					fmt.Fprintf(stdout, "%s: valid %s configuration\n", displayName(path), p.Title())
					continue
				}
				failed = true
				fmt.Fprintf(stdout, "%s: invalid %s configuration\n", displayName(path), p.Title())
				for _, i := range issues {
					fmt.Fprintf(stdout, "  - %s\n", i)
				}
			}
			if failed {
				return ExitError{Code: 1}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&provider, "provider", "p", "", "provider (detected from the file when omitted)")
	cmd.Flags().BoolVar(&latest, "latest", false, "download the latest schemas first (updates the cache)")
	cmd.Flags().BoolVar(&embedded, "embedded", false, "use the schemas built into cishift, ignoring updated copies")
	return cmd
}

func newDetectCmd(stdin io.Reader, stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "detect <file|->...",
		Short: "Print the provider a pipeline file belongs to",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, path := range args {
				data, err := readInput(path, stdin)
				if err != nil {
					return err
				}
				p, err := convert.Detect(path, data)
				if err != nil {
					return err
				}
				fmt.Fprintf(stdout, "%s: %s\n", displayName(path), p.Name())
			}
			return nil
		},
	}
}

func newProvidersCmd(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "providers",
		Short: "List supported providers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, p := range convert.Providers() {
				fmt.Fprintf(stdout, "%-10s %-20s %s\n", p.Name(), p.Title(), p.DefaultPath())
			}
			return nil
		},
	}
}
