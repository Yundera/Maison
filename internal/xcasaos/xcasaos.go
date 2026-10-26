// Package xcasaos models the CasaOS `x-casaos` compose extension and parses it
// from the raw extension map. Field set ported from casa-img's
// CasaOS-AppManagement OpenAPI structs (ComposeAppStoreInfo / AppStoreInfo).
//
// Maison's own definition is x-compose-app. x-casaos is read only for a compose
// that carries no x-compose-app at all, and then only through ComposeApp, which
// translates it into the native block — nothing else in Maison reads it.
package xcasaos

import (
	"errors"
	"strings"

	"gopkg.in/yaml.v3"
)

// ExtensionKey is the compose extension key CasaOS/Maison apps carry.
const ExtensionKey = "x-casaos"

// ErrNoExtension is returned when a compose file has no x-casaos block.
var ErrNoExtension = errors.New("x-casaos extension not found")

// StoreInfo is the app-level x-casaos metadata.
type StoreInfo struct {
	StoreAppID     string                 `yaml:"store_app_id,omitempty" json:"store_app_id,omitempty"`
	Title          map[string]string      `yaml:"title,omitempty" json:"title,omitempty"`
	Image          map[string]string      `yaml:"image,omitempty" json:"image,omitempty"`
	Description    map[string]string      `yaml:"description,omitempty" json:"description,omitempty"`
	Tagline        map[string]string      `yaml:"tagline,omitempty" json:"tagline,omitempty"`
	Icon           string                 `yaml:"icon,omitempty" json:"icon,omitempty"`
	ScreenshotLink []string               `yaml:"screenshot_link,omitempty" json:"screenshot_link,omitempty"`
	Thumbnail      string                 `yaml:"thumbnail,omitempty" json:"thumbnail,omitempty"`
	Author         string                 `yaml:"author,omitempty" json:"author,omitempty"`
	Developer      string                 `yaml:"developer,omitempty" json:"developer,omitempty"`
	Category       string                 `yaml:"category,omitempty" json:"category,omitempty"`
	Scheme         string                 `yaml:"scheme,omitempty" json:"scheme,omitempty"`
	Hostname       string                 `yaml:"hostname,omitempty" json:"hostname,omitempty"`
	PortMap        string                 `yaml:"port_map,omitempty" json:"port_map,omitempty"`
	Index          string                 `yaml:"index,omitempty" json:"index,omitempty"`
	WebUIPort      string                 `yaml:"webui_port,omitempty" json:"webui_port,omitempty"`
	Main           string                 `yaml:"main,omitempty" json:"main,omitempty"`
	MinMemory      int                    `yaml:"min_memory,omitempty" json:"min_memory,omitempty"`
	Architectures  []string               `yaml:"architectures,omitempty" json:"architectures,omitempty"`
	Tags           []string               `yaml:"tags,omitempty" json:"tags,omitempty"`
	Tips           Tips                   `yaml:"tips,omitempty" json:"tips,omitempty"`
	Apps           map[string]ServiceInfo `yaml:"apps,omitempty" json:"apps,omitempty"`

	PreInstallCmd  string `yaml:"pre-install-cmd,omitempty" json:"pre_install_cmd,omitempty"`
	PostInstallCmd string `yaml:"post-install-cmd,omitempty" json:"post_install_cmd,omitempty"`
}

// DeclaresWebUI reports whether the block names anything a click URL can be built
// from: a gateway hostname, the web-UI container port, or a host port. An index
// or scheme alone is not an address.
func (s *StoreInfo) DeclaresWebUI() bool {
	return s != nil && (strings.TrimSpace(s.Hostname) != "" ||
		strings.TrimSpace(s.WebUIPort) != "" ||
		strings.TrimSpace(s.PortMap) != "")
}

// Tips holds install-time guidance shown to the user.
type Tips struct {
	BeforeInstall map[string]string `yaml:"before_install,omitempty" json:"before_install,omitempty"`
	Custom        string            `yaml:"custom,omitempty" json:"custom,omitempty"`
}

// ServiceInfo is the per-service x-casaos metadata (documents ports/envs/etc).
type ServiceInfo struct {
	Image   string  `yaml:"image,omitempty" json:"image,omitempty"`
	Envs    []Field `yaml:"envs,omitempty" json:"envs,omitempty"`
	Ports   []Field `yaml:"ports,omitempty" json:"ports,omitempty"`
	Volumes []Field `yaml:"volumes,omitempty" json:"volumes,omitempty"`
	Devices []Field `yaml:"devices,omitempty" json:"devices,omitempty"`
}

// Field is a documented container-side value with localized descriptions.
type Field struct {
	Container   string            `yaml:"container,omitempty" json:"container,omitempty"`
	Description map[string]string `yaml:"description,omitempty" json:"description,omitempty"`
}

// Parse decodes an x-casaos extension map into StoreInfo. It uses a YAML
// round-trip so it works both on raw compose reads and on compose-go's
// preserved extension maps, without coupling to the compose engine.
func Parse(ext map[string]any) (*StoreInfo, error) {
	if ext == nil {
		return nil, ErrNoExtension
	}
	b, err := yaml.Marshal(ext)
	if err != nil {
		return nil, err
	}
	var si StoreInfo
	if err := yaml.Unmarshal(b, &si); err != nil {
		return nil, err
	}
	if si.Scheme == "" {
		si.Scheme = "http"
	}
	return &si, nil
}

// Localized returns the en_us value if present, else any value, else "".
func Localized(m map[string]string) string {
	if m == nil {
		return ""
	}
	if v, ok := m["en_us"]; ok {
		return v
	}
	for _, v := range m {
		return v
	}
	return ""
}

// ComposeApp translates the block into an x-compose-app extension map, so a
// pure CasaOS app goes through exactly the code a native one does. The one
// x-casaos field with no native key, `webui_port`, is not in the map: the
// caller carries it beside the parsed App (composefile.Resolve).
//
// CasaOS builds the click URL as scheme://hostname:port_map/index, with the
// browser's own host when hostname is empty; webui-host/-port/-scheme/-path say
// the same thing. A port_map that is the scheme's default port is dropped so the
// URL does not spell it out.
func (s *StoreInfo) ComposeApp() map[string]any {
	m := map[string]any{}
	set := func(k, v string) {
		if v = strings.TrimSpace(v); v != "" {
			m[k] = v
		}
	}
	setL := func(k string, v map[string]string) {
		if len(v) > 0 {
			m[k] = v
		}
	}
	set("id", s.StoreAppID)
	setL("title", s.Title)
	setL("tagline", s.Tagline)
	setL("description", s.Description)
	set("icon", s.Icon)
	set("thumbnail", s.Thumbnail)
	if len(s.ScreenshotLink) > 0 {
		m["screenshots"] = s.ScreenshotLink
	}
	set("category", s.Category)
	if d := strings.TrimSpace(s.Developer); d != "" {
		m["developer"] = d
	} else {
		set("developer", s.Author)
	}
	if len(s.Architectures) > 0 {
		m["architectures"] = s.Architectures
	}
	if s.MinMemory > 0 {
		m["min_memory"] = s.MinMemory
	}
	// Tags are topical, except the store's one functional tag, which is the
	// CasaOS spelling of `requires: [public-ip]`.
	for _, t := range s.Tags {
		if strings.TrimSpace(t) == NeedsPublicIPTag {
			m["requires"] = []any{"public-ip"}
		}
	}
	if t := s.tips(); len(t) > 0 {
		m["tips"] = t
	}

	scheme := strings.TrimSpace(s.Scheme)
	if scheme == "" {
		scheme = "http"
	}
	m["webui-scheme"] = scheme
	set("webui-host", s.Hostname)
	if p := strings.TrimSpace(s.PortMap); p != "" && !(scheme == "http" && p == "80") && !(scheme == "https" && p == "443") {
		m["webui-port"] = p
	}
	set("webui-path", s.Index)

	// The CasaOS install commands become `once` steps: pre-install before the
	// first start, post-install after it. composefile.Resolve marks them lenient,
	// since they were written for a plain `bash -c`.
	var steps []any
	if c := strings.TrimSpace(s.PreInstallCmd); c != "" {
		steps = append(steps, map[string]any{"name": CasaOSPreInstallStep, "at": "setup", "run": "once", "script": s.PreInstallCmd})
	}
	if c := strings.TrimSpace(s.PostInstallCmd); c != "" {
		steps = append(steps, map[string]any{"name": CasaOSPostInstallStep, "at": "started", "run": "once", "script": s.PostInstallCmd})
	}
	if len(steps) > 0 {
		m["steps"] = steps
	}
	return m
}

// NeedsPublicIPTag is the x-casaos tag the official stores use for an app that
// needs inbound connections to reach the host.
const NeedsPublicIPTag = "needs-public-ip"

// The names the translated CasaOS install commands run under — and so the names
// of their `once` markers.
const (
	CasaOSPreInstallStep  = "casaos-pre-install"
	CasaOSPostInstallStep = "casaos-post-install"
)

// tips merges the localized before_install guidance and the free-form custom note
// into the locale map x-compose-app `tips` is: one entry per before_install locale,
// with custom appended to each. custom on its own — or beside guidance with no
// en_us — becomes the en_us entry, so the map always has the fallback it needs.
func (s *StoreInfo) tips() map[string]string {
	custom := strings.TrimSpace(s.Tips.Custom)
	out := map[string]string{}
	for loc, text := range s.Tips.BeforeInstall {
		var parts []string
		if t := strings.TrimSpace(text); t != "" {
			parts = append(parts, t)
		}
		if custom != "" {
			parts = append(parts, custom)
		}
		if len(parts) > 0 {
			out[loc] = strings.Join(parts, "\n\n")
		}
	}
	if _, ok := out["en_us"]; !ok && custom != "" {
		out["en_us"] = custom
	}
	return out
}
