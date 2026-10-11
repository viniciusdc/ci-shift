package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitPusher pushes with the git command line over HTTPS. Credentials go in
// an http.extraHeader passed through GIT_CONFIG_* environment variables, so
// they never reach the command line, a config file or a credential store.
type GitPusher struct {
	// Mask, when set, is called with every secret value before use (the
	// e2e command registers them with GitHub Actions' log masking).
	Mask func(string)
}

// Push commits files into a fresh repository and pushes it as branch.
func (g GitPusher) Push(ctx context.Context, r Remote, branch string, files map[string][]byte) (string, error) {
	dir, err := os.MkdirTemp("", "cishift-e2e-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	for path, data := range files {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			return "", err
		}
	}
	steps := [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"commit", "-q", "-m", "ci-shift conformance run " + branch},
	}
	for _, args := range steps {
		if _, err := g.git(ctx, dir, Remote{}, args...); err != nil {
			return "", err
		}
	}
	commit, err := g.git(ctx, dir, Remote{}, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if _, err := g.git(ctx, dir, r, "push", "-q", r.URL, "HEAD:refs/heads/"+branch); err != nil {
		return "", err
	}
	return strings.TrimSpace(commit), nil
}

// Delete removes branch from the remote.
func (g GitPusher) Delete(ctx context.Context, r Remote, branch string) error {
	dir, err := os.MkdirTemp("", "cishift-e2e-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if _, err := g.git(ctx, dir, Remote{}, "init", "-q"); err != nil {
		return err
	}
	_, err = g.git(ctx, dir, r, "push", "-q", r.URL, "--delete", "refs/heads/"+branch)
	return err
}

// git runs one git command in dir, authenticated for r when r.Password is
// set. Output is scrubbed of the credentials before it is returned.
func (g GitPusher) git(ctx context.Context, dir string, r Remote, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(os.Environ())
	var secrets []string
	if r.Password != "" {
		header := authHeader(r)
		secrets = []string{r.Password, header, strings.TrimPrefix(header, "Authorization: Basic ")}
		if g.Mask != nil {
			for _, s := range secrets {
				g.Mask(s)
			}
		}
		cmd.Env = append(cmd.Env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0="+header,
		)
	}
	out, err := cmd.CombinedOutput()
	text := string(out)
	for _, s := range secrets {
		text = strings.ReplaceAll(text, s, "***")
	}
	if err != nil {
		return text, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(text))
	}
	return text, nil
}

// authHeader is the http.extraHeader that authenticates git for r.
func authHeader(r Remote) string {
	if r.Bearer {
		return "Authorization: Bearer " + r.Password
	}
	return "Authorization: " + basicHeader(r.Username, r.Password)
}

// gitEnv isolates git from the user's configuration (credential helpers,
// signing, hooks) and from tracing that would print headers.
func gitEnv(environ []string) []string {
	var env []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=ci-shift e2e",
		"GIT_AUTHOR_EMAIL=e2e@ci-shift.invalid",
		"GIT_COMMITTER_NAME=ci-shift e2e",
		"GIT_COMMITTER_EMAIL=e2e@ci-shift.invalid",
	)
}
