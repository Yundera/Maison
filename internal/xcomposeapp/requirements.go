package xcomposeapp

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// What an app needs from the box it runs on, in two kinds that behave differently
// (see docs/x-compose-app.md, "Requirements"):
//
//   - requires — capabilities the app cannot work without (a reachable public IP,
//     a GPU). A yes/no fact about the box; the store hides an app that lacks one.
//   - resources — quantities (cores, memory, disk) below which the app runs badly.
//     The store still shows it; below `min` installing asks for an acknowledgement,
//     below `recommended` it only gets a badge.
//
// Like the localized strings, an invalid declaration is DROPPED and recorded on
// App.Problems, never fatal: Parse's error costs the app its whole block.

// Capability names an app may list under `requires`.
const (
	CapPublicIP  = "public-ip"
	CapGPUNvidia = "gpu-nvidia"
	CapGPUDri    = "gpu-dri"
)

// Capabilities is every capability name `requires` accepts, in display order.
var Capabilities = []string{CapPublicIP, CapGPUNvidia, CapGPUDri}

// KnownCapability reports whether name is a capability `requires` accepts.
func KnownCapability(name string) bool {
	for _, c := range Capabilities {
		if c == name {
			return true
		}
	}
	return false
}

// Resources is an app's `resources` block. Either half may be absent.
type Resources struct {
	Min         *ResourceSet `yaml:"min,omitempty" json:"min,omitempty"`
	Recommended *ResourceSet `yaml:"recommended,omitempty" json:"recommended,omitempty"`
}

// ResourceSet is one tier of Resources. Zero means "not stated".
type ResourceSet struct {
	// CPU is a number of cores.
	CPU float64 `yaml:"cpu,omitempty" json:"cpu,omitempty"`
	// Memory is the box's total memory the app wants, in bytes.
	Memory int64 `yaml:"memory,omitempty" json:"memory,omitempty"`
	// Disk is the free space on the data volume the app wants at install, in bytes.
	Disk int64 `yaml:"disk,omitempty" json:"disk,omitempty"`
}

func (s *ResourceSet) empty() bool { return s == nil || (s.CPU == 0 && s.Memory == 0 && s.Disk == 0) }

// ParseSize reads a docker-style size — `512M`, `2G`, `1.5GiB`, `10g` — as bytes.
// Units are binary (1G = 1024³), as in a compose memory limit. A bare number is
// refused: `memory: 2` is far more likely a forgotten unit than two bytes.
func ParseSize(s string) (int64, error) {
	v := strings.TrimSpace(s)
	i := 0
	for i < len(v) && (v[i] >= '0' && v[i] <= '9' || v[i] == '.') {
		i++
	}
	num, unit := v[:i], strings.ToLower(strings.TrimSpace(v[i:]))
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a size", s)
	}
	unit = strings.TrimSuffix(strings.TrimSuffix(unit, "b"), "i")
	mult := map[string]float64{"k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40}[unit]
	if mult == 0 {
		return 0, fmt.Errorf("%q needs a unit: K, M, G or T", s)
	}
	return int64(math.Round(n * mult)), nil
}

// FormatSize is ParseSize's inverse for display: the largest unit that keeps one
// decimal at most — 2G, 1.5G, 512M.
func FormatSize(b int64) string {
	units := []struct {
		s string
		n float64
	}{{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}}
	for _, u := range units {
		if float64(b) >= u.n {
			v := float64(b) / u.n
			if v == math.Trunc(v) {
				return fmt.Sprintf("%.0f%s", v, u.s)
			}
			return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) + u.s
		}
	}
	return strconv.FormatInt(b, 10)
}

// checkRequirements returns ext with `requires` reduced to known capability names
// and `resources` normalized to numbers (sizes in bytes), and one problem per value
// it had to drop. ext itself is never modified.
func checkRequirements(ext map[string]any) (map[string]any, []string) {
	var problems []string
	out, owned := ext, false
	own := func() {
		if !owned {
			owned = true
			out = make(map[string]any, len(ext))
			for k, v := range ext {
				out[k] = v
			}
		}
	}

	if v, ok := ext["requires"]; ok && v != nil {
		var kept []any
		list, isList := v.([]any)
		if s, ok := v.([]string); ok { // a translated x-casaos block builds it typed
			for _, e := range s {
				list = append(list, e)
			}
			isList = true
		}
		if !isList {
			problems = append(problems, "requires: must be a list, e.g. requires: [public-ip]")
		}
		for i, e := range list {
			name, _ := e.(string)
			if !KnownCapability(name) {
				problems = append(problems, fmt.Sprintf("requires[%d]: unknown capability %v (known: %s)", i, e, strings.Join(Capabilities, ", ")))
				continue
			}
			kept = append(kept, name)
		}
		own()
		if len(kept) == 0 {
			delete(out, "requires")
		} else {
			out["requires"] = kept
		}
	}

	if v, ok := ext["resources"]; ok && v != nil {
		m, isMap := v.(map[string]any)
		norm := map[string]any{}
		if !isMap {
			problems = append(problems, "resources: must be a mapping with min and/or recommended")
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, tier := range keys {
			if tier != "min" && tier != "recommended" {
				problems = append(problems, fmt.Sprintf("resources.%s: unknown tier (known: min, recommended)", tier))
				continue
			}
			set, p := normalizeResourceSet("resources."+tier, m[tier])
			problems = append(problems, p...)
			if len(set) > 0 {
				norm[tier] = set
			}
		}
		own()
		if len(norm) == 0 {
			delete(out, "resources")
		} else {
			out["resources"] = norm
		}
	}
	return out, problems
}

func normalizeResourceSet(path string, v any) (map[string]any, []string) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, []string{path + ": must be a mapping of cpu, memory and disk"}
	}
	var problems []string
	out := map[string]any{}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := m[k]
		switch k {
		case "cpu":
			var n float64
			switch x := e.(type) {
			case int:
				n = float64(x)
			case float64:
				n = x
			case string:
				n, _ = strconv.ParseFloat(strings.TrimSpace(x), 64)
			}
			if n <= 0 {
				problems = append(problems, fmt.Sprintf("%s.cpu: must be a number of cores, e.g. cpu: 2", path))
				continue
			}
			out["cpu"] = n
		case "memory", "disk":
			s, _ := e.(string)
			b, err := ParseSize(s)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s.%s: must be a size with a unit, e.g. %s: 2G", path, k, k))
				continue
			}
			out[k] = b
		default:
			problems = append(problems, fmt.Sprintf("%s.%s: unknown resource (known: cpu, memory, disk)", path, k))
		}
	}
	return out, problems
}

// foldMinMemory carries the superseded `min_memory` (MB) into resources.min.memory
// when the block does not state one itself.
func (a *App) foldMinMemory() {
	if a.MinMemory <= 0 {
		return
	}
	if a.Resources == nil {
		a.Resources = &Resources{}
	}
	if a.Resources.Min == nil {
		a.Resources.Min = &ResourceSet{}
	}
	if a.Resources.Min.Memory == 0 {
		a.Resources.Min.Memory = int64(a.MinMemory) << 20
	}
}
