// Package lockfile records which pipeline files cishift generated and from
// what. Each entry also keeps the versions and hashes needed to verify and
// regenerate the file.
package lockfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Name is the lockfile's file name.
const Name = ".cishift.lock"

const header = "# Managed by cishift: records how generated pipeline files were produced.\n# Commit this file; see `cishift verify` and `cishift sync`.\n"

// File is a parsed lockfile.
type File struct {
	Version   int     `yaml:"version"`
	Generated []Entry `yaml:"generated"`

	path string
}

// Entry describes one generated file. Paths are relative to the lockfile's
// directory and use forward slashes.
type Entry struct {
	Output       string `yaml:"output"`
	Source       string `yaml:"source"`
	From         string `yaml:"from"`
	To           string `yaml:"to"`
	Tool         string `yaml:"cishift"`
	Schema       string `yaml:"schema,omitempty"`
	NoHeader     bool   `yaml:"no_header,omitempty"`
	SourceSHA256 string `yaml:"source_sha256"`
	OutputSHA256 string `yaml:"output_sha256"`
}

// Find returns the lockfile path for work started in dir: the nearest
// existing lockfile in dir or its parents, else a new one at the root of
// the enclosing git repository, else one in dir itself.
func Find(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	gitRoot := ""
	for d := abs; ; d = filepath.Dir(d) {
		candidate := filepath.Join(d, Name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil && gitRoot == "" {
			gitRoot = d
			break
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	if gitRoot != "" {
		return filepath.Join(gitRoot, Name), nil
	}
	return filepath.Join(abs, Name), nil
}

// Load reads a lockfile; a missing file yields an empty lockfile.
func Load(path string) (*File, error) {
	f := &File{Version: 1, path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported lockfile version %d", path, f.Version)
	}
	f.path = path
	return f, nil
}

// Path returns where the lockfile lives.
func (f *File) Path() string { return f.path }

// Dir returns the directory entry paths are relative to.
func (f *File) Dir() string { return filepath.Dir(f.path) }

// Rel converts a path to the lockfile-relative form stored in entries.
func (f *File) Rel(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(f.Dir(), abs)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// Abs converts an entry path back to a filesystem path.
func (f *File) Abs(rel string) string {
	return filepath.Join(f.Dir(), filepath.FromSlash(rel))
}

// Get returns the entry for a lockfile-relative output path.
func (f *File) Get(output string) (Entry, bool) {
	for _, e := range f.Generated {
		if e.Output == output {
			return e, true
		}
	}
	return Entry{}, false
}

// Put adds or replaces the entry for e.Output.
func (f *File) Put(e Entry) {
	for i := range f.Generated {
		if f.Generated[i].Output == e.Output {
			f.Generated[i] = e
			return
		}
	}
	f.Generated = append(f.Generated, e)
}

// Save writes the lockfile with entries sorted by output path.
func (f *File) Save() error {
	sort.Slice(f.Generated, func(i, j int) bool { return f.Generated[i].Output < f.Generated[j].Output })
	var buf bytes.Buffer
	buf.WriteString(header)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(f); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return os.WriteFile(f.path, buf.Bytes(), 0o644)
}

// Hash returns the "sha256:<hex>" digest of data.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// HashFile returns the digest of a file's contents.
func HashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return Hash(data), nil
}

// SameVersion reports whether two tool versions are the same release,
// ignoring a leading "v".
func SameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}
