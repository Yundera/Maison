// Package overrideform is the friendly, field-by-field view of an app's
// docker-compose.override.yml — the model behind the settings form.
//
// The override file stays the source of truth. Build reads it (together with the
// store's base compose) into a per-service form where every field carries both
// the store's value and the user's, and Apply writes a form back by patching the
// override's YAML *node tree* — so comments, key order, and everything the form
// doesn't model (x-compose-app, healthchecks, depends_on, …) survive a save
// untouched.
//
// # Compose merge semantics
//
// The form has to speak Compose's merge rules, because they are not uniform:
//
//   - scalars (image, restart, …) — the override replaces the base. Clearing a
//     field in the form removes the key, which falls back to the store's value.
//   - sequences (ports, volumes, devices, cap_add) — the override is *appended*
//     to the base, not substituted for it. So editing or removing a store-shipped
//     port can't be expressed by listing the ports you want: the base's would
//     still be published. When a form's list is a pure append, Apply writes just
//     the extras; otherwise it writes the whole list under Compose's `!override`
//     tag, which replaces the base's outright.
//   - mappings (environment) — merged key by key. Apply writes only the keys that
//     differ from the store's, unless the form *removes* one, which again needs
//     `!override` on the full mapping.
//
// `!override` requires Docker Compose v2.24.4 or newer.
//
// Anything the form cannot represent faithfully — a long-syntax port, a
// list-form command, a node the user already tagged by hand — is reported as
// Complex. The UI shows those read-only ("edit in the YAML view"), and Apply
// refuses to rewrite them, so the form can never silently mangle a construct it
// doesn't understand. Complexity is always recomputed from the files on save,
// never trusted from the client.
package overrideform

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yundera/maison/internal/yamlnode"
)

// Tags Compose understands in an override file.
const (
	tagOverride = "!override" // replace the base's value instead of merging into it
	tagReset    = "!reset"    // drop the base's value entirely
)

// Form is an app's override, one entry per service of the merged project.
type Form struct {
	Services []Service `json:"services"`
}

// Service is the set of fields the form can edit on one compose service. The
// common ones sit at the top; the rest are behind the UI's "Advanced" disclosure.
type Service struct {
	Name string `json:"name"`

	Image   Scalar `json:"image"`
	Restart Scalar `json:"restart"`
	Ports   List   `json:"ports"`
	Volumes List   `json:"volumes"`
	Env     EnvMap `json:"environment"`

	// Advanced
	Privileged Scalar `json:"privileged"` // "", "true" or "false"
	Command    Scalar `json:"command"`
	MemLimit   Scalar `json:"mem_limit"`
	CPUs       Scalar `json:"cpus"`
	Devices    List   `json:"devices"`
	CapAdd     List   `json:"cap_add"`
}

// Scalar is a single-valued field. Value is what runs (the override's when it
// sets one, else the store's); Base is what the store ships. An empty Value means
// "inherit from the store" — that is how a field is reset.
//
// Raw carries the field's YAML when it is Complex, so the form can still *show*
// the construct it refuses to edit rather than a blank box.
type Scalar struct {
	Value      string `json:"value"`
	Base       string `json:"base"`
	Overridden bool   `json:"overridden"`
	Complex    bool   `json:"complex"`
	Raw        string `json:"raw,omitempty"`
}

// List is a sequence field. Value is the effective list after Compose's merge,
// Base the store's — so the UI can mark each row as the store's or the user's.
type List struct {
	Value      []string `json:"value"`
	Base       []string `json:"base"`
	Overridden bool     `json:"overridden"`
	Complex    bool     `json:"complex"`
	Raw        string   `json:"raw,omitempty"`
}

// EnvVar is one environment entry, kept as an ordered pair so a save preserves
// the order the user sees.
type EnvVar struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// EnvMap is the environment field: key-merged, so Value is Base overlaid with the
// override's keys.
type EnvMap struct {
	Value      []EnvVar `json:"value"`
	Base       []EnvVar `json:"base"`
	Overridden bool     `json:"overridden"`
	Complex    bool     `json:"complex"`
	Raw        string   `json:"raw,omitempty"`
}

// Build derives the form from the store's base compose and the app's override.
func Build(base, override []byte) (*Form, error) {
	baseRoot, err := root(base)
	if err != nil {
		return nil, fmt.Errorf("docker-compose.yml: %w", err)
	}
	overRoot, err := root(override)
	if err != nil {
		return nil, fmt.Errorf("docker-compose.override.yml: %w", err)
	}

	form := &Form{}
	for _, name := range serviceNames(baseRoot, overRoot) {
		b, o := serviceNode(baseRoot, name), serviceNode(overRoot, name)
		form.Services = append(form.Services, Service{
			Name:       name,
			Image:      scalarField(b, o, "image"),
			Restart:    scalarField(b, o, "restart"),
			Ports:      listField(b, o, "ports"),
			Volumes:    listField(b, o, "volumes"),
			Env:        envField(b, o),
			Privileged: scalarField(b, o, "privileged"),
			Command:    scalarField(b, o, "command"),
			MemLimit:   scalarField(b, o, "mem_limit"),
			CPUs:       scalarField(b, o, "cpus"),
			Devices:    listField(b, o, "devices"),
			CapAdd:     listField(b, o, "cap_add"),
		})
	}
	return form, nil
}

// Apply writes a form back into the override, returning the new file. It patches
// the existing document rather than re-emitting one, so unmodified keys — and the
// comments around them — come back byte-for-byte. A nil return means the override
// has nothing left in it and the file should be removed.
//
// Only fields the form owns are touched: services it doesn't name, keys it
// doesn't model, and fields it flagged Complex are left exactly as they were.
func Apply(base, override []byte, form *Form) ([]byte, error) {
	baseRoot, err := root(base)
	if err != nil {
		return nil, fmt.Errorf("docker-compose.yml: %w", err)
	}
	overRoot, err := root(override)
	if err != nil {
		return nil, fmt.Errorf("docker-compose.override.yml: %w", err)
	}

	for i := range form.Services {
		svc := &form.Services[i]
		b := serviceNode(baseRoot, svc.Name)
		o := ensureService(overRoot, svc.Name)

		applyScalar(b, o, "image", svc.Image.Value)
		applyScalar(b, o, "restart", svc.Restart.Value)
		applyList(b, o, "ports", svc.Ports.Value)
		applyList(b, o, "volumes", svc.Volumes.Value)
		applyEnv(b, o, svc.Env.Value)
		applyScalar(b, o, "privileged", svc.Privileged.Value)
		applyScalar(b, o, "command", svc.Command.Value)
		applyScalar(b, o, "mem_limit", svc.MemLimit.Value)
		applyScalar(b, o, "cpus", svc.CPUs.Value)
		applyList(b, o, "devices", svc.Devices.Value)
		applyList(b, o, "cap_add", svc.CapAdd.Value)

		// A service the form emptied out carries no override any more. Dropping it
		// (and, below, an empty `services:` block) is what lets an override shrink
		// back to nothing when every field is reset to the store's default.
		if len(o.Content) == 0 {
			mapDelete(mapGet(overRoot, "services"), svc.Name)
		}
	}
	if svcs := mapGet(overRoot, "services"); svcs != nil && len(svcs.Content) == 0 {
		mapDelete(overRoot, "services")
	}
	if len(overRoot.Content) == 0 {
		return nil, nil
	}

	return yamlnode.Encode(overRoot)
}

// --- field readers ---

func scalarField(b, o *yaml.Node, key string) Scalar {
	f := Scalar{}
	bn := mapGet(b, key)
	f.Base, f.Complex = scalarValue(bn)
	f.Value = f.Base

	if on := mapGet(o, key); on != nil {
		f.Overridden = true
		v, complex := scalarValue(on)
		f.Value, f.Complex = v, f.Complex || complex
	}
	if f.Complex {
		f.Value, f.Base, f.Raw = "", "", rawField(b, o, key)
	}
	return f
}

func listField(b, o *yaml.Node, key string) List {
	f := List{}
	f.Base, f.Complex = stringList(mapGet(b, key))
	f.Value = append([]string(nil), f.Base...)

	if on := mapGet(o, key); on != nil {
		f.Overridden = true
		items, complex := stringList(on)
		f.Complex = f.Complex || complex
		switch on.Tag {
		case tagReset:
			f.Value = nil
		case tagOverride:
			f.Value = items
		default:
			f.Value = append(f.Value, items...) // Compose appends; so does the form
		}
	}
	if f.Complex {
		f.Value, f.Base, f.Raw = nil, nil, rawField(b, o, key)
	}
	return f
}

func envField(b, o *yaml.Node) EnvMap {
	f := EnvMap{}
	f.Base, f.Complex = envPairs(mapGet(b, "environment"))
	f.Value = append([]EnvVar(nil), f.Base...)

	if on := mapGet(o, "environment"); on != nil {
		f.Overridden = true
		items, complex := envPairs(on)
		f.Complex = f.Complex || complex
		switch on.Tag {
		case tagReset:
			f.Value = nil
		case tagOverride:
			f.Value = items
		default:
			for _, e := range items { // key merge, like Compose's
				f.Value = setEnv(f.Value, e)
			}
		}
	}
	if f.Complex {
		f.Value, f.Base, f.Raw = nil, nil, rawField(b, o, "environment")
	}
	return f
}

// rawField renders a field the form won't edit, preferring the override's node —
// that is the one the operator would go and change.
func rawField(b, o *yaml.Node, key string) string {
	n := mapGet(o, key)
	if n == nil {
		n = mapGet(b, key)
	}
	return renderNode(n)
}

// --- field writers ---

// applyScalar sets the key, or removes it when the form's value is empty or just
// restates the store's — an empty field means "inherit", which is how the UI's
// reset works.
func applyScalar(b, o *yaml.Node, key, want string) {
	if scalarField(b, o, key).Complex {
		return
	}
	base, _ := scalarValue(mapGet(b, key))
	want = strings.TrimSpace(want)
	if want == "" || want == base {
		mapDelete(o, key)
		return
	}
	mapSet(o, key, scalarNode(want))
}

// applyList writes the smallest override that produces the wanted list: nothing
// when it matches the store's, just the extra entries when it only appends to it,
// and the full list under `!override` when it edits or drops one of the store's
// (which Compose would otherwise keep, since it concatenates sequences).
func applyList(b, o *yaml.Node, key string, want []string) {
	if listField(b, o, key).Complex {
		return
	}
	base, _ := stringList(mapGet(b, key))
	want = compact(want)

	if equal(want, base) {
		mapDelete(o, key)
		return
	}
	if extra, ok := appendOnly(base, want); ok {
		mapSet(o, key, seqNode(extra, ""))
		return
	}
	mapSet(o, key, seqNode(want, tagOverride))
}

// applyEnv writes only the variables that differ from the store's — unless the
// form drops one of the store's, which a key merge cannot express and so needs
// the whole mapping under `!override`.
func applyEnv(b, o *yaml.Node, want []EnvVar) {
	if envField(b, o).Complex {
		return
	}
	base, _ := envPairs(mapGet(b, "environment"))
	want = compactEnv(want)

	if equalEnv(want, base) {
		mapDelete(o, "environment")
		return
	}
	for _, e := range base {
		if _, ok := lookupEnv(want, e.Key); !ok {
			mapSet(o, "environment", envNode(want, tagOverride)) // a store variable was removed
			return
		}
	}
	var delta []EnvVar
	for _, e := range want {
		if v, ok := lookupEnv(base, e.Key); ok && v == e.Value {
			continue
		}
		delta = append(delta, e)
	}
	if len(delta) == 0 {
		mapDelete(o, "environment")
		return
	}
	mapSet(o, "environment", envNode(delta, ""))
}

// --- node readers ---

// root returns a compose file's top-level mapping, empty when the file is.
func root(b []byte) (*yaml.Node, error) { return yamlnode.Root(b) }

// serviceNames lists the project's services, base order first so the form's tabs
// match the store's file, then any the override adds on its own.
func serviceNames(base, over *yaml.Node) []string {
	var names []string
	seen := map[string]bool{}
	for _, m := range []*yaml.Node{mapGet(base, "services"), mapGet(over, "services")} {
		if m == nil || m.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(m.Content); i += 2 {
			if n := m.Content[i].Value; !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	return names
}

func serviceNode(rootNode *yaml.Node, name string) *yaml.Node {
	svc := mapGet(mapGet(rootNode, "services"), name)
	if svc == nil || svc.Kind != yaml.MappingNode {
		return nil
	}
	return svc
}

// ensureService returns the override's mapping for a service, creating it (and
// the `services` block) if the override doesn't mention it yet.
func ensureService(rootNode *yaml.Node, name string) *yaml.Node {
	return yamlnode.EnsureMap(yamlnode.EnsureMap(rootNode, "services"), name)
}

// scalarValue reads a plain scalar. A node that is anything else (a list-form
// command, a mapping, a hand-written tag) is complex: the form shows its YAML —
// see rawField — but won't rewrite it.
func scalarValue(n *yaml.Node) (string, bool) {
	if n == nil {
		return "", false
	}
	if n.Kind != yaml.ScalarNode || strings.HasPrefix(n.Tag, "!") && !strings.HasPrefix(n.Tag, "!!") {
		return "", true
	}
	return n.Value, false
}

// stringList reads a sequence of plain scalars. Long-syntax entries (a port
// written as a mapping) make the whole field complex.
func stringList(n *yaml.Node) ([]string, bool) {
	if n == nil {
		return nil, false
	}
	if n.Kind != yaml.SequenceNode {
		return nil, true
	}
	var out []string
	complex := false
	for _, item := range n.Content {
		if item.Kind != yaml.ScalarNode {
			complex = true
			continue
		}
		out = append(out, item.Value)
	}
	return out, complex
}

// envPairs reads an environment block in either compose form: a `KEY: value`
// mapping or a `- KEY=value` sequence.
func envPairs(n *yaml.Node) ([]EnvVar, bool) {
	if n == nil {
		return nil, false
	}
	switch n.Kind {
	case yaml.MappingNode:
		var out []EnvVar
		complex := false
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if v.Kind != yaml.ScalarNode {
				complex = true
				continue
			}
			out = append(out, EnvVar{Key: k.Value, Value: v.Value})
		}
		return out, complex
	case yaml.SequenceNode:
		var out []EnvVar
		complex := false
		for _, item := range n.Content {
			if item.Kind != yaml.ScalarNode {
				complex = true
				continue
			}
			k, v, _ := strings.Cut(item.Value, "=")
			out = append(out, EnvVar{Key: k, Value: v})
		}
		return out, complex
	default:
		return nil, true
	}
}

// renderNode re-emits a node as YAML, so the form can *show* a construct it
// refuses to edit.
func renderNode(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	out, err := yaml.Marshal(n)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// --- node writers ---

func mapGet(m *yaml.Node, key string) *yaml.Node      { return yamlnode.Get(m, key) }
func mapSet(m *yaml.Node, key string, val *yaml.Node) { yamlnode.Set(m, key, val) }
func mapDelete(m *yaml.Node, key string)              { yamlnode.Delete(m, key) }
func scalarNode(v string) *yaml.Node                  { return yamlnode.Scalar(v) }

func seqNode(items []string, tag string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	if tag != "" {
		n.Tag = tag
		n.Style = yaml.TaggedStyle
	}
	for _, item := range items {
		n.Content = append(n.Content, scalarNode(item))
	}
	return n
}

func envNode(vars []EnvVar, tag string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if tag != "" {
		n.Tag = tag
		n.Style = yaml.TaggedStyle
	}
	for _, e := range vars {
		n.Content = append(n.Content, scalarNode(e.Key), scalarNode(e.Value))
	}
	return n
}

// --- list helpers ---

// appendOnly reports whether want is base with entries added at the end — the
// case Compose's own concatenation already handles, so the override only has to
// carry the extras.
func appendOnly(base, want []string) ([]string, bool) {
	if len(want) <= len(base) {
		return nil, false
	}
	for i, v := range base {
		if want[i] != v {
			return nil, false
		}
	}
	return want[len(base):], true
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalEnv(a, b []EnvVar) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func lookupEnv(vars []EnvVar, key string) (string, bool) {
	for _, e := range vars {
		if e.Key == key {
			return e.Value, true
		}
	}
	return "", false
}

func setEnv(vars []EnvVar, e EnvVar) []EnvVar {
	for i := range vars {
		if vars[i].Key == e.Key {
			vars[i].Value = e.Value
			return vars
		}
	}
	return append(vars, e)
}

// compact drops the blank rows an "+ Add" form inevitably leaves behind.
func compact(items []string) []string {
	var out []string
	for _, v := range items {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func compactEnv(vars []EnvVar) []EnvVar {
	var out []EnvVar
	for _, e := range vars {
		if k := strings.TrimSpace(e.Key); k != "" {
			out = append(out, EnvVar{Key: k, Value: e.Value})
		}
	}
	return out
}
