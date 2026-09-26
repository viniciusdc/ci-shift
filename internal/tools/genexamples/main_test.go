package main

import (
	"strings"
	"testing"
)

func TestParseExample(t *testing.T) {
	data := []byte("# doc:id    demo\n# doc:file  README.md\n# doc:tested true\n\nexec tool\n\n-- in.txt --\nx\n-- display.sh --\ntool\n# ok\n")
	meta, display := parseExample(data)
	if meta["id"] != "demo" || meta["file"] != "README.md" || meta["tested"] != "true" {
		t.Errorf("meta = %v", meta)
	}
	if display != "tool\n# ok" {
		t.Errorf("display = %q", display)
	}
}

// freezeLike mimics the parts of freeze --window output the post-processing
// relies on.
const freezeLike = `<?xml version="1.0" encoding="UTF-8"?>
<svg width="760.00" height="100.00" xmlns="http://www.w3.org/2000/svg">
<style></style><rect width="760.00" height="100.00" fill="#0d1117" rx="8.00" ry="8.00" x="0.00px" y="0.00px"/>
<g><text>tool</text></g>
<svg x="0.00px" y="0.00px"><circle cx="13.50" cy="12.00" r="5.50" fill="#FF5A54"/><circle cx="32.50" cy="12.00" r="5.50" fill="#E6BF29"/><circle cx="51.50" cy="12.00" r="5.50" fill="#52C12B"/></svg></svg>`

func TestPostProcess(t *testing.T) {
	svg, err := addWindowTitle(freezeLike, "my-demo", true)
	if err != nil {
		t.Fatal(err)
	}
	svg = addCIFooter(svg, "/x/my-demo.txtar", true, true)

	for _, want := range []string{
		`<svg width="760.00" height="136.00"`,               // canvas grew by the footer
		`width="760.00" height="136.00" fill="#161b22" rx=`, // window bg recoloured and stretched
		`#52C12B"/><text x="380.00"`,                        // title after the last light
		">my demo</text>",
		"✓ CI tested · my-demo.txtar",
	} {
		if !strings.Contains(svg, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if _, err := addWindowTitle("<svg></svg>", "x", true); err == nil {
		t.Error("want an error for unexpected input")
	}
}

func TestStampChangesWithContent(t *testing.T) {
	a := stampFor("id", "console", "one", "f.txtar", true, 760)
	b := stampFor("id", "console", "two", "f.txtar", true, 760)
	c := stampFor("id", "bash", "one", "f.txtar", true, 760)
	if a == b || a == c || !strings.HasPrefix(a, stampPrefix) {
		t.Errorf("stamps %q, %q and %q", a, b, c)
	}
}

func TestColorSession(t *testing.T) {
	got := colorSession("$ hello greet Ada\n# Hi, Ada!\n\n$ hello count", true)
	lines := strings.Split(got, "\n")
	if len(lines) != 4 || lines[2] != "" {
		t.Fatalf("line structure changed: %q", lines)
	}
	cmd := "\x1b[38;2;139;148;158m$ \x1b[38;2;230;237;243mhello greet Ada\x1b[0m"
	if lines[0] != cmd {
		t.Errorf("command line = %q, want %q", lines[0], cmd)
	}
	// Output starting with "#" is still output, not a command.
	if lines[1] != "\x1b[38;2;139;148;158m# Hi, Ada!\x1b[0m" {
		t.Errorf("output line = %q", lines[1])
	}
	if light := colorSession("$ x", false); !strings.Contains(light, "31;35;40") {
		t.Errorf("light theme command color missing: %q", light)
	}
}
