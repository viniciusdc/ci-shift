package runner

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/viniciusdc/ci-shift/e2e/conformance"
)

// Select builds the candidates for the named providers from environment
// variables (see e2e/README.md for the names). A provider with any
// required variable unset is returned with Missing filled in, to be
// skipped. secret is the E2E_SECRET value set on the providers that take
// it through their API.
func Select(names []string, env func(string) string, secret string) ([]Candidate, error) {
	var out []Candidate
	seen := map[string]bool{}
	for _, raw := range names {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		c := Candidate{Name: name}
		switch name {
		case "gitlab":
			p, miss := newGitLab(env)
			p.Secret = secret
			c.P, c.Missing = p, miss
		case "bitbucket":
			p, miss := newBitbucket(env)
			p.Secret = secret
			c.P, c.Missing = p, miss
		case "azure":
			p, miss := newAzure(env)
			p.Secret = secret
			c.P, c.Missing = p, miss
		case "circleci":
			p, miss := newCircleCI(env)
			p.Secret = secret
			c.P, c.Missing = p, miss
		default:
			return nil, fmt.Errorf("unknown provider %q (known: %s)", raw, strings.Join(conformance.Targets, ", "))
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no providers selected")
	}
	return out, nil
}

// NewSecret returns a random value for E2E_SECRET: 32 hex characters, which
// satisfies GitLab's rules for masked variables.
func NewSecret() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// missing returns the names whose value is empty, sorted.
func missing(vars map[string]string) []string {
	var out []string
	for k, v := range vars {
		if strings.TrimSpace(v) == "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func or(v, def string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return def
}
