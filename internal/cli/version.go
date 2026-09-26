package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/viniciusdc/ci-shift/internal/buildinfo"
)

// latestReleaseURL is the GitHub API endpoint of the newest release.
var latestReleaseURL = "https://api.github.com/repos/viniciusdc/ci-shift/releases/latest"

func newVersionCmd(stdout io.Writer) *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the cishift version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			current := buildinfo.Version()
			fmt.Fprintf(stdout, "cishift %s\n", current)
			if !check {
				return nil
			}
			latest, err := latestRelease()
			if err != nil {
				return fmt.Errorf("could not check for a newer release: %w", err)
			}
			switch {
			case latest == "":
				fmt.Fprintln(stdout, "no releases published yet")
			case current == "dev":
				fmt.Fprintf(stdout, "latest release: %s (this is a development build)\n", latest)
			case newer(latest, current):
				fmt.Fprintf(stdout, "a newer release is available: %s\n  go install github.com/viniciusdc/ci-shift/cmd/cishift@%s\n", latest, latest)
			default:
				fmt.Fprintln(stdout, "you are on the latest release")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "check GitHub for a newer release (the only command that goes online)")
	return cmd
}

func latestRelease() (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(latestReleaseURL)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub API: %s", resp.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return body.TagName, nil
}

// newer reports whether version a is newer than b (vMAJOR.MINOR.PATCH; any
// pre-release suffix is ignored).
func newer(a, b string) bool {
	pa, pb := parts(a), parts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parts(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "-")
	for i, p := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}
