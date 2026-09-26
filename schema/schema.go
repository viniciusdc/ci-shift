// Package schema validates CI configuration files against the providers'
// published JSON schemas.
//
// Snapshots of every schema are embedded so validation works offline and
// deterministically. Latest can be set to download fresh copies (cached
// for a day in the user cache directory).
package schema

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/viniciusdc/cistela/internal/yamlx"
)

//go:embed snapshots/*.json
var snapshots embed.FS

// Source describes where a provider's schema is published.
type Source struct {
	Provider string
	URL      string
}

// file is the snapshot/cache file name of a source.
func (s Source) file() string { return s.Provider + ".json" }

// Sources lists the schema of every supported provider.
var Sources = []Source{
	{Provider: "github", URL: "https://raw.githubusercontent.com/SchemaStore/schemastore/refs/heads/master/src/schemas/json/github-workflow.json"},
	{Provider: "gitlab", URL: "https://gitlab.com/gitlab-org/gitlab/-/raw/master/app/assets/javascripts/editor/schema/ci.json"},
	{Provider: "circleci", URL: "https://raw.githubusercontent.com/CircleCI-Public/circleci-yaml-language-server/main/schema.json"},
	{Provider: "azure", URL: "https://raw.githubusercontent.com/microsoft/azure-pipelines-vscode/main/service-schema.json"},
	{Provider: "bitbucket", URL: "https://api.bitbucket.org/schemas/pipelines-configuration"},
}

// Options control where schemas come from.
type Options struct {
	// Latest downloads the current schema instead of the embedded snapshot.
	Latest bool
	// CacheDir overrides the download cache location.
	CacheDir string
	// HTTPClient overrides the client used for downloads.
	HTTPClient *http.Client
}

// Issue is a single validation problem.
type Issue struct {
	Location string // JSON pointer into the document, e.g. /jobs/build
	Message  string
}

func (i Issue) String() string {
	loc := i.Location
	if loc == "" {
		loc = "/"
	}
	return loc + ": " + i.Message
}

var (
	compiled   = map[string]*jsonschema.Schema{}
	compiledMu sync.Mutex
)

func sourceFor(provider string) (Source, error) {
	for _, s := range Sources {
		if s.Provider == provider {
			return s, nil
		}
	}
	return Source{}, fmt.Errorf("no schema known for provider %q", provider)
}

// Load returns the compiled schema for a provider.
func Load(provider string, opts Options) (*jsonschema.Schema, error) {
	src, err := sourceFor(provider)
	if err != nil {
		return nil, err
	}
	key := provider
	if opts.Latest {
		key += "@latest"
	}
	compiledMu.Lock()
	defer compiledMu.Unlock()
	if s, ok := compiled[key]; ok {
		return s, nil
	}
	var data []byte
	if opts.Latest {
		data, err = download(src, opts)
		if err != nil {
			return nil, err
		}
	} else {
		data, err = snapshots.ReadFile("snapshots/" + src.file())
		if err != nil {
			return nil, err
		}
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse %s schema: %w", provider, err)
	}
	sanitize(doc)
	c := jsonschema.NewCompiler()
	c.UseLoader(offlineLoader{})
	c.UseRegexpEngine(lenientRegexp)
	if err := c.AddResource(src.URL, doc); err != nil {
		return nil, err
	}
	s, err := c.Compile(src.URL)
	if err != nil {
		return nil, fmt.Errorf("compile %s schema: %w", provider, err)
	}
	compiled[key] = s
	return s, nil
}

// offlineLoader refuses to fetch referenced documents: every supported
// schema is self-contained.
type offlineLoader struct{}

func (offlineLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("schema references external document %s", url)
}

func download(src Source, opts Options) ([]byte, error) {
	dir := opts.CacheDir
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			base = os.TempDir()
		}
		dir = filepath.Join(base, "cistela", "schemas")
	}
	cached := filepath.Join(dir, src.file())
	if st, err := os.Stat(cached); err == nil && time.Since(st.ModTime()) < 24*time.Hour {
		return os.ReadFile(cached)
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Get(src.URL)
	if err != nil {
		return nil, fmt.Errorf("download %s schema: %w", src.Provider, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s schema: %s", src.Provider, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("download %s schema: response is not JSON", src.Provider)
	}
	if err := os.MkdirAll(dir, 0o755); err == nil {
		_ = os.WriteFile(cached, data, 0o644)
	}
	return data, nil
}

// Validate checks a YAML document against the provider's schema. It
// returns the problems found; err is only set when validation could not run.
func Validate(provider string, data []byte, opts Options) ([]Issue, error) {
	s, err := Load(provider, opts)
	if err != nil {
		return nil, err
	}
	root, err := yamlx.Load(data)
	if err != nil {
		return []Issue{{Message: "invalid YAML: " + err.Error()}}, nil
	}
	v, err := yamlx.ToAny(root)
	if err != nil {
		return nil, err
	}
	// Round-trip through JSON so numbers and maps have the types the
	// validator expects.
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	err = s.Validate(inst)
	if err == nil {
		return nil, nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return nil, err
	}
	return issues(ve), nil
}

var printer = message.NewPrinter(language.English)

// issues flattens a validation error tree into the most specific problems:
// leaf errors whose location is not a prefix of another leaf's location.
func issues(ve *jsonschema.ValidationError) []Issue {
	var leaves []*jsonschema.ValidationError
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			leaves = append(leaves, e)
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	byLoc := map[string][]string{}
	for _, l := range leaves {
		loc := pointer(l.InstanceLocation)
		msg := l.ErrorKind.LocalizedString(printer)
		byLoc[loc] = append(byLoc[loc], msg)
	}
	locs := make([]string, 0, len(byLoc))
	for loc := range byLoc {
		locs = append(locs, loc)
	}
	sort.Strings(locs)
	var out []Issue
	for _, loc := range locs {
		deeper := false
		for _, other := range locs {
			if other != loc && strings.HasPrefix(other, loc+"/") {
				deeper = true
				break
			}
		}
		if deeper {
			continue
		}
		seen := map[string]bool{}
		for _, msg := range byLoc[loc] {
			if !seen[msg] {
				seen[msg] = true
				out = append(out, Issue{Location: loc, Message: msg})
			}
		}
	}
	return out
}

func pointer(tokens []string) string {
	var b strings.Builder
	for _, t := range tokens {
		b.WriteByte('/')
		b.WriteString(strings.NewReplacer("~", "~0", "/", "~1").Replace(t))
	}
	return b.String()
}

// sanitize fixes known defects in published schemas that would make them
// fail meta-validation. CircleCI lists both "2.1" and 2.1 in an enum; the
// validator treats those as duplicates when checking the schema but not
// when validating documents, so such enums become an anyOf of consts.
func sanitize(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if arr, ok := val.([]any); ok && k == "enum" && looseDuplicates(arr) {
				var alts []any
				for _, item := range arr {
					alts = append(alts, map[string]any{"const": item})
				}
				delete(t, "enum")
				t["anyOf"] = alts
				continue
			}
			sanitize(val)
		}
	case []any:
		for _, item := range t {
			sanitize(item)
		}
	}
}

func looseDuplicates(arr []any) bool {
	seen := map[string]bool{}
	for _, v := range arr {
		key := fmt.Sprint(v)
		if seen[key] {
			return true
		}
		seen[key] = true
	}
	return false
}
