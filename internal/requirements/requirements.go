// Package requirements decides whether an app can run on this box: it holds what
// Maison knows of the box (Host) and compares an app's declared requirements
// against it (Check). See docs/x-compose-app.md, "Requirements".
//
// The two kinds behave differently, and that difference is the whole design:
//
//   - A missing capability (public IP, GPU) means the app cannot work. The store
//     hides it, and installing it anyway takes an explicit acknowledgement.
//   - A missing resource (cores, memory, disk) means it works badly. Below `min`
//     installing takes the same acknowledgement; below `recommended` it only gets
//     a badge.
//
// An architecture the app's images do not ship is the one hard refusal: there is
// nothing to acknowledge, the containers would not start.
//
// A capability Maison could not determine never hides anything. Detection can be
// wrong, and a store that silently empties because a probe failed is worse than one
// that shows an app with a note.
package requirements

import (
	"fmt"
	"strconv"

	"github.com/yundera/maison/internal/xcomposeapp"
)

// Tri is a fact that may be unknown.
type Tri string

const (
	Yes     Tri = "yes"
	No      Tri = "no"
	Unknown Tri = "unknown"
)

// Override values of the capabilities setting.
const (
	Auto     = "auto"
	ForceYes = "yes"
	ForceNo  = "no"
)

// ValidOverride reports whether v is a value the capabilities setting accepts.
// Empty reads as auto.
func ValidOverride(v string) bool { return v == "" || v == Auto || v == ForceYes || v == ForceNo }

// Probe is what detection found for one capability, and why.
type Probe struct {
	Value Tri `json:"value"`
	// Source says how Value was found, for the settings page — e.g. "address
	// 203.0.113.10 is on a host interface". Never localized: it is a diagnostic.
	Source string `json:"source,omitempty"`
}

// Capability is one capability as the box has it: detected, overridden, effective.
type Capability struct {
	Name      string `json:"name"`
	Detected  Probe  `json:"detected"`
	Setting   string `json:"setting"`
	Effective Tri    `json:"effective"`
}

// Host is what the box offers. A zero quantity means "not measured" and is never
// held against an app.
type Host struct {
	Arch         string       `json:"arch"`
	CPU          int          `json:"cpu"`
	Memory       int64        `json:"memory"`
	DiskFree     int64        `json:"disk_free"`
	Capabilities []Capability `json:"capabilities"`
}

// Detected is the raw detection a Host is built from.
type Detected struct {
	Arch     string
	CPU      int
	Memory   int64
	DiskFree int64
	Probes   map[string]Probe
}

// NewHost combines detection with the operator's overrides (capability name →
// auto|yes|no). Every known capability gets a row, in xcomposeapp.Capabilities
// order, whether or not it was probed.
func NewHost(d Detected, overrides map[string]string) Host {
	h := Host{Arch: d.Arch, CPU: d.CPU, Memory: d.Memory, DiskFree: d.DiskFree}
	for _, name := range xcomposeapp.Capabilities {
		p, ok := d.Probes[name]
		if !ok || p.Value == "" {
			p.Value = Unknown
		}
		c := Capability{Name: name, Detected: p, Setting: overrides[name], Effective: p.Value}
		switch c.Setting {
		case ForceYes:
			c.Effective = Yes
		case ForceNo:
			c.Effective = No
		default:
			c.Setting = Auto
		}
		h.Capabilities = append(h.Capabilities, c)
	}
	return h
}

// Has returns the effective value of capability name.
func (h Host) Has(name string) Tri {
	for _, c := range h.Capabilities {
		if c.Name == name {
			return c.Effective
		}
	}
	return Unknown
}

// Reason is one requirement the box does not meet. Code is a capability name, or
// "arch", "cpu", "memory", "disk". Need and Have are display values (`2G`,
// `amd64, arm64`), never localized.
type Reason struct {
	Code string `json:"code"`
	Need string `json:"need,omitempty"`
	Have string `json:"have,omitempty"`
}

// Result is how an app fits this box. The zero value fits.
type Result struct {
	// Blocked is an architecture mismatch: never installable.
	Blocked bool `json:"blocked,omitempty"`
	// Unsupported are capabilities the box effectively lacks (and the arch, when
	// Blocked).
	Unsupported []Reason `json:"unsupported,omitempty"`
	// BelowMin are resources under the app's stated minimum.
	BelowMin []Reason `json:"below_min,omitempty"`
	// BelowRecommended are resources under the app's recommendation.
	BelowRecommended []Reason `json:"below_recommended,omitempty"`
	// Uncertain are required capabilities Maison could not determine. They hide
	// nothing and gate nothing; the store mentions them.
	Uncertain []string `json:"uncertain,omitempty"`
}

// Hidden reports whether the store hides the app by default.
func (r Result) Hidden() bool { return r.Blocked || len(r.Unsupported) > 0 }

// NeedsAck reports whether installing takes an explicit acknowledgement.
func (r Result) NeedsAck() bool { return len(r.Unsupported) > 0 || len(r.BelowMin) > 0 }

// Fits reports whether nothing at all is short.
func (r Result) Fits() bool {
	return !r.Blocked && len(r.Unsupported) == 0 && len(r.BelowMin) == 0 && len(r.BelowRecommended) == 0 && len(r.Uncertain) == 0
}

// Codes lists the codes of every reason that gates installing, for messages.
func (r Result) Codes() []string {
	var out []string
	for _, l := range [][]Reason{r.Unsupported, r.BelowMin} {
		for _, x := range l {
			out = append(out, x.Code)
		}
	}
	return out
}

// Check compares app's requirements with h.
func Check(app *xcomposeapp.App, h Host) Result {
	var r Result
	if app == nil {
		return r
	}
	if len(app.Architectures) > 0 && h.Arch != "" && !archSupported(app.Architectures, h.Arch) {
		r.Blocked = true
		r.Unsupported = append(r.Unsupported, Reason{Code: "arch", Need: join(app.Architectures), Have: h.Arch})
	}
	for _, name := range app.Requires {
		switch h.Has(name) {
		case No:
			r.Unsupported = append(r.Unsupported, Reason{Code: name})
		case Unknown:
			r.Uncertain = append(r.Uncertain, name)
		}
	}
	if app.Resources != nil {
		r.BelowMin = short(app.Resources.Min, h)
		r.BelowRecommended = short(app.Resources.Recommended, h)
		// Below min is the stronger statement; don't repeat it as below recommended.
		r.BelowRecommended = without(r.BelowRecommended, r.BelowMin)
	}
	return r
}

// short lists the quantities of s that h does not reach. An unmeasured host value
// (zero) is never short.
func short(s *xcomposeapp.ResourceSet, h Host) []Reason {
	if s == nil {
		return nil
	}
	var out []Reason
	if s.CPU > 0 && h.CPU > 0 && float64(h.CPU) < s.CPU {
		out = append(out, Reason{Code: "cpu", Need: strconv.FormatFloat(s.CPU, 'f', -1, 64), Have: strconv.Itoa(h.CPU)})
	}
	// Total memory as the OS reports it sits a little under the nominal size (the
	// kernel's own reservation), so a "4G" box reads ~3.8G. Allow 10% so a box of
	// exactly the stated size is not called short.
	if s.Memory > 0 && h.Memory > 0 && float64(h.Memory) < float64(s.Memory)*0.9 {
		out = append(out, Reason{Code: "memory", Need: xcomposeapp.FormatSize(s.Memory), Have: xcomposeapp.FormatSize(h.Memory)})
	}
	if s.Disk > 0 && h.DiskFree > 0 && h.DiskFree < s.Disk {
		out = append(out, Reason{Code: "disk", Need: xcomposeapp.FormatSize(s.Disk), Have: xcomposeapp.FormatSize(h.DiskFree)})
	}
	return out
}

func without(rs, drop []Reason) []Reason {
	var out []Reason
outer:
	for _, r := range rs {
		for _, d := range drop {
			if d.Code == r.Code {
				continue outer
			}
		}
		out = append(out, r)
	}
	return out
}

// archSupported matches the host's Go architecture against an app's list, which
// stores write in Docker's spelling (amd64, arm64, arm) — the same as Go's, plus a
// few aliases seen in the wild.
func archSupported(list []string, arch string) bool {
	for _, a := range list {
		if normArch(a) == normArch(arch) {
			return true
		}
	}
	return false
}

func normArch(a string) string {
	switch a {
	case "x86_64", "x86-64":
		return "amd64"
	case "aarch64", "arm64/v8":
		return "arm64"
	case "armv7", "armv7l", "arm/v7", "armhf":
		return "arm"
	}
	return a
}

func join(l []string) string {
	s := ""
	for i, v := range l {
		if i > 0 {
			s += ", "
		}
		s += v
	}
	return s
}

// Error is the refusal an install returns when the app does not fit and was not
// acknowledged (or cannot be).
type Error struct{ Result Result }

func (e *Error) Error() string {
	if e.Result.Blocked {
		return fmt.Sprintf("this app does not run on %s", e.Result.Unsupported[0].Have)
	}
	return fmt.Sprintf("this box does not meet the app's requirements: %v", e.Result.Codes())
}
