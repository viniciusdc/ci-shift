// Package cli implements the cistela command line.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/viniciusdc/cistela/convert"
	"github.com/viniciusdc/cistela/ir"
	"github.com/viniciusdc/cistela/schema"
)

// Version is set at build time with -ldflags "-X .../internal/cli.Version=...".
var Version = "dev"

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
	root := &cobra.Command{
		Use:   "cistela",
		Short: "Translate CI/CD pipelines between providers",
		Long: `cistela translates CI/CD configurations between GitHub Actions, GitLab CI/CD,
CircleCI, Azure Pipelines and Bitbucket Pipelines through a shared pipeline
model, and validates them against each provider's JSON schema.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}
	root.AddCommand(
		newConvertCmd(stdin, stdout, stderr),
		newValidateCmd(stdin, stdout, stderr),
		newDetectCmd(stdin, stdout),
		newProvidersCmd(stdout),
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

func newConvertCmd(stdin io.Reader, stdout, stderr io.Writer) *cobra.Command {
	var from, to, output string
	var noHeader, strict, check, quiet bool
	cmd := &cobra.Command{
		Use:   "convert <file|->",
		Short: "Convert a pipeline to another provider",
		Example: `  cistela convert .gitlab-ci.yml --to github -o .github/workflows/ci.yml
  cistela convert .circleci/config.yml --to azure
  cat bitbucket-pipelines.yml | cistela convert - --from bitbucket --to gitlab`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := readInput(args[0], stdin)
			if err != nil {
				return err
			}
			src, err := resolveProvider(from, args[0], data)
			if err != nil {
				return err
			}
			dst, err := convert.Get(to)
			if err != nil {
				return err
			}
			res, err := convert.Convert(data, src, dst, convert.Options{
				Header:     !noHeader,
				SourceName: displayName(args[0]),
			})
			if err != nil {
				return err
			}
			if !quiet {
				for _, d := range res.Diagnostics {
					fmt.Fprintln(stderr, d)
				}
			}
			if output == "" || output == "-" {
				if _, err := stdout.Write(res.Output); err != nil {
					return err
				}
			} else {
				if dir := filepath.Dir(output); dir != "." {
					if err := os.MkdirAll(dir, 0o755); err != nil {
						return err
					}
				}
				if err := os.WriteFile(output, res.Output, 0o644); err != nil {
					return err
				}
				if !quiet {
					fmt.Fprintf(stderr, "wrote %s (%s -> %s)\n", output, src.Title(), dst.Title())
				}
			}
			failed := false
			if check {
				issues, err := schema.Validate(dst.Name(), res.Output, schema.Options{})
				if err != nil {
					return err
				}
				for _, i := range issues {
					fmt.Fprintf(stderr, "schema: %s\n", i)
				}
				failed = len(issues) > 0
			}
			if strict {
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
		},
	}
	f := cmd.Flags()
	f.StringVarP(&from, "from", "f", "", "source provider (detected from the file when omitted)")
	f.StringVarP(&to, "to", "t", "", "target provider: "+strings.Join(convert.Names(), ", "))
	f.StringVarP(&output, "output", "o", "", "output file (default stdout)")
	f.BoolVar(&noHeader, "no-header", false, "omit the review comment header")
	f.BoolVar(&strict, "strict", false, "exit 1 when the conversion produced warnings")
	f.BoolVar(&check, "check", false, "validate the output against the target provider's schema")
	f.BoolVarP(&quiet, "quiet", "q", false, "do not print diagnostics")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func displayName(path string) string {
	if path == "-" {
		return "stdin"
	}
	return filepath.ToSlash(path)
}

func newValidateCmd(stdin io.Reader, stdout, stderr io.Writer) *cobra.Command {
	var provider string
	var latest bool
	cmd := &cobra.Command{
		Use:   "validate <file|->...",
		Short: "Validate pipeline files against their provider's JSON schema",
		Args:  cobra.MinimumNArgs(1),
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
				issues, err := schema.Validate(p.Name(), data, schema.Options{Latest: latest})
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
	cmd.Flags().BoolVar(&latest, "latest", false, "download the latest schema instead of the embedded snapshot")
	_ = stderr
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
