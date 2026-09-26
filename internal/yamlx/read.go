// Package yamlx provides small helpers for reading YAML documents in an
// order-preserving way and for writing ordered YAML back out.
package yamlx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load parses data into a document tree with aliases and merge keys (<<)
// resolved, so callers never see AliasNodes. It returns the root content
// node (usually a mapping) or nil for an empty document.
func Load(data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, nil
	}
	return resolve(doc.Content[0], 0)
}

const maxDepth = 256

// resolve returns a deep copy of n with aliases expanded and merge keys applied.
func resolve(n *yaml.Node, depth int) (*yaml.Node, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("yaml nesting too deep (alias loop?) at line %d", n.Line)
	}
	switch n.Kind {
	case yaml.AliasNode:
		return resolve(n.Alias, depth+1)
	case yaml.ScalarNode:
		c := *n
		return &c, nil
	case yaml.SequenceNode:
		c := *n
		c.Content = nil
		for _, item := range n.Content {
			r, err := resolve(item, depth+1)
			if err != nil {
				return nil, err
			}
			c.Content = append(c.Content, r)
		}
		return &c, nil
	case yaml.MappingNode:
		c := *n
		c.Content = nil
		var merged []*yaml.Node // key/value pairs coming from << merges
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind == yaml.ScalarNode && k.Value == "<<" && k.Tag != "!!str" {
				rv, err := resolve(v, depth+1)
				if err != nil {
					return nil, err
				}
				sources := []*yaml.Node{rv}
				if rv.Kind == yaml.SequenceNode {
					sources = rv.Content
				}
				for _, src := range sources {
					if src.Kind != yaml.MappingNode {
						return nil, fmt.Errorf("line %d: merge key value must be a mapping", v.Line)
					}
					for j := 0; j+1 < len(src.Content); j += 2 {
						if indexOf(merged, src.Content[j].Value) < 0 {
							merged = append(merged, src.Content[j], src.Content[j+1])
						}
					}
				}
				continue
			}
			rk, err := resolve(k, depth+1)
			if err != nil {
				return nil, err
			}
			rv, err := resolve(v, depth+1)
			if err != nil {
				return nil, err
			}
			c.Content = append(c.Content, rk, rv)
		}
		// Explicit keys win over merged ones.
		for j := 0; j+1 < len(merged); j += 2 {
			if indexOf(c.Content, merged[j].Value) < 0 {
				c.Content = append(c.Content, merged[j], merged[j+1])
			}
		}
		return &c, nil
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return resolve(n.Content[0], depth+1)
	}
	return n, nil
}

func indexOf(pairs []*yaml.Node, key string) int {
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i].Value == key {
			return i
		}
	}
	return -1
}

// IsMap reports whether n is a mapping node.
func IsMap(n *yaml.Node) bool { return n != nil && n.Kind == yaml.MappingNode }

// IsSeq reports whether n is a sequence node.
func IsSeq(n *yaml.Node) bool { return n != nil && n.Kind == yaml.SequenceNode }

// IsScalar reports whether n is a scalar node.
func IsScalar(n *yaml.Node) bool { return n != nil && n.Kind == yaml.ScalarNode }

// IsNull reports whether n is absent or an explicit null.
func IsNull(n *yaml.Node) bool {
	return n == nil || (n.Kind == yaml.ScalarNode && n.Tag == "!!null")
}

// Get returns the value for key in mapping n, or nil.
func Get(n *yaml.Node, key string) *yaml.Node {
	if !IsMap(n) {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// Has reports whether mapping n contains key.
func Has(n *yaml.Node, key string) bool {
	if !IsMap(n) {
		return false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return true
		}
	}
	return false
}

// Path walks nested mapping keys.
func Path(n *yaml.Node, keys ...string) *yaml.Node {
	for _, k := range keys {
		n = Get(n, k)
		if n == nil {
			return nil
		}
	}
	return n
}

// Pair is a key/value entry of a mapping.
type Pair struct {
	Key   string
	Value *yaml.Node
}

// Pairs returns the entries of mapping n in document order.
func Pairs(n *yaml.Node) []Pair {
	if !IsMap(n) {
		return nil
	}
	out := make([]Pair, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, Pair{Key: n.Content[i].Value, Value: n.Content[i+1]})
	}
	return out
}

// Keys returns the keys of mapping n in document order.
func Keys(n *yaml.Node) []string {
	var out []string
	for _, p := range Pairs(n) {
		out = append(out, p.Key)
	}
	return out
}

// Items returns the items of sequence n (or nil).
func Items(n *yaml.Node) []*yaml.Node {
	if !IsSeq(n) {
		return nil
	}
	return n.Content
}

// Str returns the scalar value of n, or "" when n is not a scalar.
func Str(n *yaml.Node) string {
	if !IsScalar(n) || n.Tag == "!!null" {
		return ""
	}
	return n.Value
}

// StrList returns a list of strings from a scalar or a sequence of scalars.
// Nested sequences are flattened (GitLab allows nested script arrays).
func StrList(n *yaml.Node) []string {
	switch {
	case n == nil:
		return nil
	case IsScalar(n):
		if n.Tag == "!!null" {
			return nil
		}
		return []string{n.Value}
	case IsSeq(n):
		var out []string
		for _, item := range n.Content {
			out = append(out, StrList(item)...)
		}
		return out
	}
	return nil
}

// Bool interprets a scalar as a boolean (YAML 1.1 spellings accepted).
func Bool(n *yaml.Node) (value bool, ok bool) {
	if !IsScalar(n) {
		return false, false
	}
	switch strings.ToLower(n.Value) {
	case "true", "yes", "on", "y":
		return true, true
	case "false", "no", "off", "n":
		return false, true
	}
	return false, false
}

// Int interprets a scalar as an integer.
func Int(n *yaml.Node) (int, bool) {
	if !IsScalar(n) {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(n.Value))
	if err != nil {
		return 0, false
	}
	return v, true
}

// ToAny converts a node into plain Go values (map[string]any keeps no order;
// use it only where order does not matter, e.g. for JSON schema validation).
func ToAny(n *yaml.Node) (any, error) {
	if n == nil {
		return nil, nil
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, err
	}
	return normalize(v), nil
}

func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = normalize(val)
		}
		return t
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[fmt.Sprint(k)] = normalize(val)
		}
		return m
	case []any:
		for i := range t {
			t[i] = normalize(t[i])
		}
		return t
	}
	return v
}

// Key returns a plain string scalar node, for building nodes by hand.
func Key(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}
