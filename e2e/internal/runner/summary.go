package runner

import (
	"fmt"
	"io"
	"strings"
)

// Summary writes the results as Markdown: one overview table, then the
// per-job checks of every provider that ran. It reads well in a terminal
// and renders as tables in $GITHUB_STEP_SUMMARY.
func Summary(w io.Writer, results []Result) {
	fmt.Fprintln(w, "## ci-shift live provider tests")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| Provider | Result | Run | Time | Details |")
	fmt.Fprintln(w, "|---|---|---|---|---|")
	for _, r := range results {
		run := "–"
		if r.Run != nil && r.Run.URL != "" {
			run = fmt.Sprintf("[%s](%s)", cell(r.Run.ID), r.Run.URL)
		}
		elapsed := "–"
		if r.Elapsed > 0 {
			elapsed = r.Elapsed.String()
		}
		details := r.Reason
		if r.Outcome == OutcomeDryRun {
			details = strings.TrimSpace(details + "; files: " + strings.Join(r.Files, ", "))
		}
		if details == "" {
			details = "–"
		}
		fmt.Fprintf(w, "| %s | %s %s | %s | %s | %s |\n", r.Provider, icon(r.Outcome), r.Outcome, run, elapsed, cell(details))
	}
	for _, r := range results {
		if len(r.Checks) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n### %s jobs\n\n", r.Provider)
		fmt.Fprintln(w, "| Job | Verifies | Reported as | OK |")
		fmt.Fprintln(w, "|---|---|---|---|")
		for _, c := range r.Checks {
			var got []string
			for _, g := range c.Got {
				got = append(got, fmt.Sprintf("%s: %s", g.Name, g.Raw))
			}
			reported := strings.Join(got, "<br>")
			if reported == "" {
				reported = "–"
			}
			ok := "✅"
			if !c.OK {
				ok = "❌ " + c.Note
			}
			fmt.Fprintf(w, "| `%s` | %s | %s | %s |\n", c.Job.ID, cell(c.Job.Checks), cell(reported), cell(ok))
		}
		for _, e := range r.Extra {
			fmt.Fprintf(w, "| %s | (not in the conformance pipeline) | %s | – |\n", cell(e.Name), cell(e.Raw))
		}
	}
}

func icon(o Outcome) string {
	switch o {
	case OutcomePassed:
		return "✅"
	case OutcomeFailed:
		return "❌"
	case OutcomeSkipped:
		return "⏭️"
	}
	return "📝"
}

// cell makes s safe inside a Markdown table cell.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", "<br>")
}
