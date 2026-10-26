package usersettings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/yundera/maison/internal/domains"
	"github.com/yundera/maison/internal/notify"
	"github.com/yundera/maison/internal/routing"
	"github.com/yundera/maison/internal/secrets"
)

// MANAGED SETTINGS
//
// A setting the deployment can own. While it is managed, the deployment's value is
// the only one: the dashboard shows it read-only with the owner's name, a user edit
// of it is dropped (Set), and the deployment re-asserts it on every self-check. When
// the deployment stops offering the feature it releases the setting, which clears
// the value and hands the setting back to the user.
//
// One setting, one home: there is never a deployment value and a user value for the
// same thing at the same time — the managed flag says whose the one value is.
//
// The only way in or out of managed is Manage / Release, which the server exposes
// on its container-local admin socket and never over HTTP: anything that can reach
// the dashboard's API — every app on a shared network — must not be able to change
// who owns a setting.
//
// On disk a MANAGED field is wrapped: {"managed": true, "owner": "...", "value": ...}.
// An unmanaged one keeps its plain shape — a plain routing or smtp object —
// which is what every Maison before managed settings reads: a box rolled back to an
// older image keeps the user's settings instead of failing to parse the file and
// running on defaults. In memory and over the API it stays flat, with the owners in
// Settings.Managed, so nothing that reads a setting's value had to change.

// ManagedNames are the settings a deployment may own, in a stable order.
var ManagedNames = []string{"capabilities", "feedback", "onboarding", "routing", "smtp"}

// ErrUnknownSetting is a name that is not in ManagedNames.
var ErrUnknownSetting = errors.New("unknown setting")

// ownerRe is what an owner may be called: a name shown to the user ("Managed by
// Yundera"), so a leading capital is allowed, but no spaces or punctuation.
var ownerRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,31}$`)

// Feedback is where the dashboard's "Send feedback" goes (internal/feedback). Never
// user-editable: it names the operator's endpoint and its token.
type Feedback struct {
	URL   string `json:"url"`
	Token string `json:"token,omitempty"`
}

// Onboarding is the deployment's first-run check (internal/onboarding): while set,
// every browser navigation asks CheckURL whether setup is owed, and a "show" answer
// redirects the browser to the deployment's setup page. Token, when set, is sent as a
// bearer token and may be a secret reference. Never user-editable, for the reason the
// onboarding package gives — a client-controlled state transition would let anything
// on the network mark the box set up.
//
// A value from before the check ({"url": …}) still decodes from disk, as an empty
// CheckURL, and that means off.
type Onboarding struct {
	CheckURL string `json:"check_url"`
	Token    string `json:"token,omitempty"`
}

// IsManaged reports whether setting name is owned by the deployment, and by whom.
func (s Settings) IsManaged(name string) (string, bool) {
	o, ok := s.Managed[name]
	return o, ok
}

// Manage makes setting name the deployment's, with value raw (JSON), owned by owner.
//
// The value is decoded strictly — an unknown field is an error, not something to
// ignore — and validated with the same rules the dashboard's own editor applies.
// A deployment script that sends something this version does not understand fails
// loudly, which is the point: it is the version check the env vars never had.
//
// has reports whether a referenced secret exists; a value that points at a missing
// secret is refused, so the deployment must store the secret first.
func (s *Store) Manage(name, owner string, raw json.RawMessage, has func(string) bool) error {
	if !ownerRe.MatchString(owner) {
		return fmt.Errorf("invalid owner %q: use letters, digits and -, starting with a letter", owner)
	}
	apply, err := decodeManaged(name, raw)
	if err != nil {
		return err
	}
	var probe Settings
	apply(&probe)
	for _, ref := range probe.secretRefs(name) {
		if has == nil || !has(ref) {
			return fmt.Errorf("%s references secret %q, which is not set — store it first", name, ref)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cur
	next.Managed = cloneOwners(s.cur.Managed)
	apply(&next)
	next.Managed[name] = owner
	if err := s.write(next); err != nil {
		return err
	}
	s.cur = next
	return nil
}

// Release hands setting name back to the user: the value is cleared and the setting
// is no longer managed. It returns the secrets the released value referenced, for
// the caller to delete — they belonged to the deployment's value, which is gone.
//
// Releasing a setting that is not managed does nothing. That is what makes it safe
// for a deployment to call on every self-check while its feature is off: a value
// the user set themselves is never cleared by it.
func (s *Store) Release(name string) ([]string, error) {
	if !knownManaged(name) {
		return nil, fmt.Errorf("%w %q (known: %s)", ErrUnknownSetting, name, strings.Join(ManagedNames, ", "))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cur.Managed[name]; !ok {
		return nil, nil
	}
	refs := s.cur.secretRefs(name)
	next := s.cur
	next.Managed = cloneOwners(s.cur.Managed)
	delete(next.Managed, name)
	clearField(&next, name)
	if err := s.write(next); err != nil {
		return nil, err
	}
	s.cur = next
	return refs, nil
}

// decodeManaged validates raw for setting name and returns the assignment to make.
func decodeManaged(name string, raw json.RawMessage) (func(*Settings), error) {
	switch name {
	case "smtp":
		var v notify.SMTP
		if err := strictDecode(raw, &v); err != nil {
			return nil, err
		}
		if v.Host = strings.TrimSpace(v.Host); v.Host == "" {
			return nil, errors.New("smtp: host is required")
		}
		if v.Port == 0 {
			v.Port = 587
		}
		if v.Port < 1 || v.Port > 65535 {
			return nil, fmt.Errorf("smtp: port %d is out of range", v.Port)
		}
		return func(st *Settings) { st.SMTP = &v }, nil
	case "routing":
		var v routing.Setting
		if err := strictDecode(raw, &v); err != nil {
			return nil, err
		}
		v, err := routing.Normalize(v)
		if err != nil {
			return nil, fmt.Errorf("routing: %w", err)
		}
		return func(st *Settings) { st.Routing = &v }, nil
	case "capabilities":
		var v map[string]string
		if err := strictDecode(raw, &v); err != nil {
			return nil, err
		}
		if err := ValidateCapabilities(v); err != nil {
			return nil, err
		}
		return func(st *Settings) { st.Capabilities = v }, nil
	case "feedback":
		var v Feedback
		if err := strictDecode(raw, &v); err != nil {
			return nil, err
		}
		if err := httpURL(v.URL); err != nil {
			return nil, fmt.Errorf("feedback: url %w", err)
		}
		if v.Token == "" {
			return nil, errors.New("feedback: token is required")
		}
		return func(st *Settings) { st.Feedback = &v }, nil
	case "onboarding":
		var v Onboarding
		if err := strictDecode(raw, &v); err != nil {
			return nil, err
		}
		if err := httpURL(v.CheckURL); err != nil {
			return nil, fmt.Errorf("onboarding: check_url %w", err)
		}
		return func(st *Settings) { st.Onboarding = &v }, nil
	}
	return nil, fmt.Errorf("%w %q (known: %s)", ErrUnknownSetting, name, strings.Join(ManagedNames, ", "))
}

// secretRefs lists the secrets setting name's current value references. Only the
// fields that can hold a secret are looked at — a password, a token.
func (s Settings) secretRefs(name string) []string {
	var vals []string
	switch name {
	case "smtp":
		if s.SMTP != nil {
			vals = append(vals, s.SMTP.Pass)
		}
	case "feedback":
		if s.Feedback != nil {
			vals = append(vals, s.Feedback.Token)
		}
	case "onboarding":
		if s.Onboarding != nil {
			vals = append(vals, s.Onboarding.Token)
		}
	}
	var refs []string
	for _, v := range vals {
		if n, ok := secrets.IsRef(v); ok {
			refs = append(refs, n)
		}
	}
	return refs
}

// dropManaged removes from an incoming user edit every field the deployment owns.
// Dropped silently rather than refused: the dashboard sends its whole settings
// object on every change, managed fields included, and a wallpaper change must
// not fail because the SMTP block it carried along is read-only.
func dropManaged(in *Settings, owned map[string]string) {
	for name := range owned {
		clearField(in, name)
	}
	in.Managed = nil
}

func clearField(st *Settings, name string) {
	switch name {
	case "smtp":
		st.SMTP = nil
	case "routing":
		st.Routing = nil
	case "feedback":
		st.Feedback = nil
	case "onboarding":
		st.Onboarding = nil
	case "capabilities":
		st.Capabilities = nil
	}
}

func knownManaged(name string) bool {
	for _, n := range ManagedNames {
		if n == name {
			return true
		}
	}
	return false
}

func cloneOwners(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func strictDecode(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid value: %w", err)
	}
	if dec.More() {
		return errors.New("invalid value: trailing data")
	}
	return nil
}

func httpURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an absolute http(s) URL, got %q", raw)
	}
	return nil
}

// ── on-disk encoding ────────────────────────────────────────────────────────────

// wrapped is one manageable field as it sits in settings.json.
type wrapped struct {
	Managed bool            `json:"managed"`
	Owner   string          `json:"owner,omitempty"`
	Value   json.RawMessage `json:"value"`
}

// decodeFile reads settings.json. Each manageable field may be in the wrapped shape
// or in its plain shape — a plain smtp object, a bare routing object. Either way it
// comes out flat, with the owners in Managed. (Maison 1.1.50 wrapped unmanaged values
// too, with "managed": false; those still read, and are written back plain.)
//
// migrated reports a file from before the routing setting whose additional-domains
// list was turned into one (routing.Migrate), for the caller to write back.
func decodeFile(b []byte) (st Settings, migrated bool, err error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		return Settings{}, false, err
	}
	var legacy []domains.Domain
	var legacyOwner string
	var legacyManaged bool
	if raw, ok := doc["domains"]; ok {
		delete(doc, "domains")
		migrated = true
		value, owner, managed := unwrap(raw)
		if len(value) > 0 && string(value) != "null" {
			if err := json.Unmarshal(value, &legacy); err != nil {
				return Settings{}, false, fmt.Errorf("domains: %w", err)
			}
		}
		legacyOwner, legacyManaged = owner, managed
	}
	fields := map[string]json.RawMessage{}
	for _, n := range ManagedNames {
		if raw, ok := doc[n]; ok {
			fields[n] = raw
			delete(doc, n)
		}
	}
	delete(doc, "managed") // the API's view, never trusted from disk
	rest, err := json.Marshal(doc)
	if err != nil {
		return Settings{}, false, err
	}
	if err := json.Unmarshal(rest, &st); err != nil {
		return Settings{}, false, err
	}
	for name, raw := range fields {
		value, owner, managed := unwrap(raw)
		if err := setRaw(&st, name, value); err != nil {
			return Settings{}, false, fmt.Errorf("%s: %w", name, err)
		}
		if managed {
			if st.Managed == nil {
				st.Managed = map[string]string{}
			}
			st.Managed[name] = owner
		}
	}
	if st.Routing == nil {
		if r := routing.Migrate(legacy); r != nil {
			st.Routing = r
			if legacyManaged {
				if st.Managed == nil {
					st.Managed = map[string]string{}
				}
				st.Managed["routing"] = legacyOwner
			}
		}
	}
	return st, migrated, nil
}

// unwrap tells the wrapped shape from a legacy flat one: wrapped is an object with
// both a "managed" and a "value" key, which no legacy value has.
func unwrap(raw json.RawMessage) (value json.RawMessage, owner string, managed bool) {
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) == nil {
		_, hasManaged := probe["managed"]
		_, hasValue := probe["value"]
		if hasManaged && hasValue {
			var w wrapped
			if json.Unmarshal(raw, &w) == nil {
				return w.Value, w.Owner, w.Managed
			}
		}
	}
	return raw, "", false
}

func setRaw(st *Settings, name string, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	switch name {
	case "smtp":
		return json.Unmarshal(raw, &st.SMTP)
	case "routing":
		return json.Unmarshal(raw, &st.Routing)
	case "feedback":
		return json.Unmarshal(raw, &st.Feedback)
	case "onboarding":
		return json.Unmarshal(raw, &st.Onboarding)
	case "capabilities":
		return json.Unmarshal(raw, &st.Capabilities)
	}
	return nil
}

// encodeFile renders settings.json: every field in its plain shape, except a managed
// one, which is wrapped with its owner. A field with no value and no owner is left out.
func encodeFile(st Settings) ([]byte, error) {
	owners := st.Managed
	flat := st
	flat.Managed = nil
	flat.SMTP, flat.Routing, flat.Feedback, flat.Onboarding, flat.Capabilities = nil, nil, nil, nil, nil
	b, err := json.Marshal(flat)
	if err != nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	values := map[string]any{}
	if st.SMTP != nil {
		values["smtp"] = st.SMTP
	}
	if st.Routing != nil {
		values["routing"] = st.Routing
	}
	if st.Feedback != nil {
		values["feedback"] = st.Feedback
	}
	if st.Onboarding != nil {
		values["onboarding"] = st.Onboarding
	}
	if st.Capabilities != nil {
		values["capabilities"] = st.Capabilities
	}
	names := append([]string(nil), ManagedNames...)
	sort.Strings(names)
	for _, n := range names {
		v, hasValue := values[n]
		owner, managed := owners[n]
		if !hasValue && !managed {
			continue
		}
		vb, err := json.Marshal(v) // nil marshals as null
		if err != nil {
			return nil, err
		}
		if !managed {
			doc[n] = vb
			continue
		}
		wb, err := json.Marshal(wrapped{Managed: managed, Owner: owner, Value: vb})
		if err != nil {
			return nil, err
		}
		doc[n] = wb
	}
	return json.MarshalIndent(doc, "", "  ")
}
