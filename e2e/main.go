// Package main runs the conformance pipeline on real CI providers.
//
// It converts e2e/conformance/ci.yml to each selected provider, pushes the
// result to a fresh branch of that provider's throwaway repository, waits
// for the run and checks every job, then deletes the branch. Providers
// without configuration are reported as skipped. See e2e/README.md.
//
//	go run ./e2e -dry-run
//	go run ./e2e -providers gitlab,azure -timeout 20m
//
// It is a test harness, not part of the cishift binary.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/viniciusdc/ci-shift/e2e/conformance"
	"github.com/viniciusdc/ci-shift/e2e/internal/runner"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, env func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("e2e", flag.ContinueOnError)
	fs.SetOutput(stderr)
	providers := fs.String("providers", or(env("E2E_PROVIDERS"), strings.Join(conformance.Targets, ",")), "comma-separated providers to test")
	timeout := fs.Duration("timeout", 30*time.Minute, "per-provider limit from push to finished run")
	runID := fs.String("run-id", defaultRunID(env), "unique part of the test branch names (e2e/<run-id>-<provider>)")
	dryRun := fs.Bool("dry-run", false, "convert and validate, print what would be pushed; no network")
	out := fs.String("out", "", "with -dry-run, also write the files that would be pushed to this directory")
	summary := fs.String("summary", env("GITHUB_STEP_SUMMARY"), "append the Markdown summary to this file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected arguments: %v\n", fs.Args())
		return 2
	}

	actions := env("GITHUB_ACTIONS") == "true"
	mask := func(s string) {
		if actions && s != "" {
			fmt.Fprintf(stdout, "::add-mask::%s\n", s)
		}
	}
	secret := env("E2E_SECRET")
	if secret == "" && !*dryRun {
		var err error
		if secret, err = runner.NewSecret(); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
	}
	mask(secret)

	cands, err := runner.Select(strings.Split(*providers, ","), env, secret)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	for _, c := range cands {
		if len(c.Missing) == 0 {
			for _, s := range secretsOf(c.P) {
				mask(s)
			}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	results := runner.Execute(ctx, cands, runner.GitPusher{Mask: mask}, runner.Options{
		RunID:   *runID,
		Timeout: *timeout,
		DryRun:  *dryRun,
		Log:     stderr,
	})
	if *dryRun && *out != "" {
		if err := writeFiles(*out, cands); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stderr, "wrote the converted pipelines under %s\n", *out)
	}

	fmt.Fprintln(stdout)
	runner.Summary(stdout, results)
	if *summary != "" {
		f, err := os.OpenFile(*summary, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		runner.Summary(f, results)
		if err := f.Close(); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
	}
	if runner.Failed(results) {
		return 1
	}
	return 0
}

// secretsOf returns the credentials of a configured provider, for masking.
func secretsOf(p runner.Provider) []string {
	r := p.Remote()
	out := []string{r.Password}
	if s, ok := p.(interface{ Secrets() []string }); ok {
		out = append(out, s.Secrets()...)
	}
	return out
}

// writeFiles writes every candidate's pushed files under dir/<provider>/.
func writeFiles(dir string, cands []runner.Candidate) error {
	for _, c := range cands {
		files, err := runner.Prepare(c)
		if err != nil {
			return err
		}
		for path, data := range files {
			full := filepath.Join(dir, c.Name, filepath.FromSlash(path))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(full, data, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// defaultRunID identifies this run: the GitHub Actions run and attempt, or
// the local time.
func defaultRunID(env func(string) string) string {
	if id := env("GITHUB_RUN_ID"); id != "" {
		return id + "-" + or(env("GITHUB_RUN_ATTEMPT"), "1")
	}
	return "local-" + time.Now().UTC().Format("20060102-150405")
}

func or(v, def string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return def
}
