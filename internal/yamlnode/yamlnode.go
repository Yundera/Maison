// Package yamlnode holds the small helpers Maison uses to patch a YAML
// document in place, through its node tree.
//
// The alternative — unmarshal into map[string]any, edit, re-marshal — flattens
// everything it round-trips: comments vanish, keys get re-sorted, and the indent
// changes. That is fine for a file Maison owns outright, but the compose
// override is shared with the operator (they edit it by hand, in the YAML view),
// so writes to it have to leave the parts Maison didn't touch byte-for-byte
// intact. These helpers are what make that possible.
package yamlnode

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Root parses b and returns its top-level mapping. Empty (or whitespace-only)
// input yields an empty mapping rather than an error, so a caller can treat "no
// file yet" and "empty file" the same way.
func Root(b []byte) (*yaml.Node, error) {
	empty := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if len(strings.TrimSpace(string(b))) == 0 {
		return empty, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return empty, nil
	}
	n := doc.Content[0]
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("top level is not a mapping")
	}
	return n, nil
}

// Get returns the value node stored under key, or nil.
func Get(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// KeyNode returns the *key* node for key (the one comments hang off), or nil.
func KeyNode(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i]
		}
	}
	return nil
}

// Keys lists the mapping's keys in document order.
func Keys(m *yaml.Node) []string {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]string, 0, len(m.Content)/2)
	for i := 0; i+1 < len(m.Content); i += 2 {
		out = append(out, m.Content[i].Value)
	}
	return out
}

// Set replaces a key's value in place — keeping its position, and the comments
// attached to it — or appends the key when it is new.
func Set(m *yaml.Node, key string, val *yaml.Node) {
	if m == nil {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			val.HeadComment = m.Content[i+1].HeadComment
			val.LineComment = m.Content[i+1].LineComment
			val.FootComment = m.Content[i+1].FootComment
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, Scalar(key), val)
}

// Delete removes key and its value, along with any comments attached to them.
func Delete(m *yaml.Node, key string) {
	if m == nil {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// EnsureMap returns the mapping stored under key, creating it if it is missing
// (or if what is there is not a mapping).
func EnsureMap(m *yaml.Node, key string) *yaml.Node {
	if m == nil {
		return nil
	}
	if sub := Get(m, key); sub != nil && sub.Kind == yaml.MappingNode {
		return sub
	}
	sub := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	Set(m, key, sub)
	return sub
}

// Scalar builds a plain string scalar node.
func Scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// StringSeq builds a flow-style sequence of strings ([a, b, c]) — compact enough
// for a generated list to stay readable inside a file a human also reads.
func StringSeq(vals []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, v := range vals {
		n.Content = append(n.Content, Scalar(v))
	}
	return n
}

// Encode re-emits a document at two-space indent. yaml.Marshal's default is
// four, which would silently re-indent a hand-written file on its first save.
func Encode(root *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
