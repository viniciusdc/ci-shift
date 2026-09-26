package yamlx

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Map is an insertion-ordered mapping used to build YAML output.
type Map struct {
	keys []string
	vals map[string]any
}

// NewMap returns an empty ordered map.
func NewMap() *Map { return &Map{vals: map[string]any{}} }

// Set stores key=value, keeping the original position when the key exists.
func (m *Map) Set(key string, value any) *Map {
	if _, ok := m.vals[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.vals[key] = value
	return m
}

// SetIf stores key=value unless value is a zero value (empty string, nil,
// empty slice/map, false, 0).
func (m *Map) SetIf(key string, value any) *Map {
	if isZero(value) {
		return m
	}
	return m.Set(key, value)
}

// Get returns the value for key.
func (m *Map) Get(key string) (any, bool) {
	v, ok := m.vals[key]
	return v, ok
}

// Delete removes key.
func (m *Map) Delete(key string) {
	if _, ok := m.vals[key]; !ok {
		return
	}
	delete(m.vals, key)
	for i, k := range m.keys {
		if k == key {
			m.keys = append(m.keys[:i], m.keys[i+1:]...)
			break
		}
	}
}

// Len returns the number of entries.
func (m *Map) Len() int {
	if m == nil {
		return 0
	}
	return len(m.keys)
}

// Keys returns the keys in insertion order.
func (m *Map) Keys() []string { return append([]string(nil), m.keys...) }

func isZero(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case bool:
		return !t
	case int:
		return t == 0
	case []any:
		return len(t) == 0
	case []string:
		return len(t) == 0
	case *Map:
		return t == nil || t.Len() == 0
	case Comment:
		return t.Value == nil || isZero(t.Value)
	}
	return false
}

// Comment wraps a value and attaches a head comment to its key or item.
type Comment struct {
	Text  string
	Value any
}

// Flow wraps a sequence or mapping so it is rendered in flow style ([a, b]).
type Flow struct{ Value any }

// Quoted forces a string to be rendered double-quoted.
type Quoted string

// Literal forces a string to be rendered as a literal block (|).
type Literal string

// Marshal renders v (built from *Map, []any, []string, strings, numbers and
// booleans) as YAML with two-space indentation. headComment, when non-empty,
// is emitted at the top of the document.
func Marshal(v any, headComment string) ([]byte, error) {
	n, err := toNode(v)
	if err != nil {
		return nil, err
	}
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{n}}
	if headComment != "" {
		doc.HeadComment = headComment
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func toNode(v any) (*yaml.Node, error) {
	switch t := v.(type) {
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: ""}, nil
	case *yaml.Node:
		return t, nil
	case *Map:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		if t == nil {
			return n, nil
		}
		for _, k := range t.keys {
			val := t.vals[k]
			kn := keyNode(k)
			if c, ok := val.(Comment); ok {
				kn.HeadComment = c.Text
				val = c.Value
			}
			vn, err := toNode(val)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			n.Content = append(n.Content, kn, vn)
		}
		return n, nil
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, item := range t {
			comment := ""
			if c, ok := item.(Comment); ok {
				comment = c.Text
				item = c.Value
			}
			in, err := toNode(item)
			if err != nil {
				return nil, err
			}
			if comment != "" {
				in.HeadComment = comment
			}
			n.Content = append(n.Content, in)
		}
		return n, nil
	case []string:
		items := make([]any, len(t))
		for i, s := range t {
			items[i] = s
		}
		return toNode(items)
	case Flow:
		n, err := toNode(t.Value)
		if err != nil {
			return nil, err
		}
		n.Style = yaml.FlowStyle
		return n, nil
	case Comment:
		n, err := toNode(t.Value)
		if err != nil {
			return nil, err
		}
		n.HeadComment = t.Text
		return n, nil
	case Quoted:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: string(t), Style: yaml.DoubleQuotedStyle}, nil
	case Literal:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: string(t), Style: yaml.LiteralStyle}, nil
	case string:
		return strNode(t), nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(t)}, nil
	case int:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(t)}, nil
	case float64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(t, 'f', -1, 64)}, nil
	}
	return nil, fmt.Errorf("yamlx: unsupported value type %T", v)
}

// strNode renders a string scalar, using a literal block for multi-line
// strings and quoting whenever a plain scalar would be read back as another
// type or be ambiguous.
func strNode(s string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
	switch {
	case strings.Contains(strings.TrimRight(s, "\n"), "\n"):
		n.Style = yaml.LiteralStyle
	case needsQuotes(s):
		if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
			n.Style = yaml.DoubleQuotedStyle
		} else {
			n.Style = yaml.SingleQuotedStyle
		}
	}
	return n
}

func needsQuotes(s string) bool {
	if s == "" {
		return true
	}
	// Values that YAML 1.1 or 1.2 parsers could read as non-strings.
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "y", "n", "null", "~":
		return true
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0o") {
		return true
	}
	// Let the encoder decide for everything else (it quotes indicators etc.).
	var probe any
	if err := yaml.Unmarshal([]byte(s), &probe); err != nil {
		return true
	}
	if str, ok := probe.(string); !ok || str != s {
		return true
	}
	return false
}

// keyNode renders a mapping key. Keys are always strings to the tools that
// read CI configs, so boolean-looking words such as GitHub's "on" stay plain.
func keyNode(s string) *yaml.Node {
	switch strings.ToLower(s) {
	case "on", "off", "yes", "no", "y", "n", "true", "false":
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
	}
	return strNode(s)
}
