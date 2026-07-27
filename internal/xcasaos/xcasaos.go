// Package xcasaos models the CasaOS `x-casaos` compose extension and parses it
// from the raw extension map. Field set ported from casa-img's
// CasaOS-AppManagement OpenAPI structs (ComposeAppStoreInfo / AppStoreInfo).
package xcasaos

import (
	"errors"

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
	Tips           Tips                   `yaml:"tips,omitempty" json:"tips,omitempty"`
	Apps           map[string]ServiceInfo `yaml:"apps,omitempty" json:"apps,omitempty"`

	PreInstallCmd  string `yaml:"pre-install-cmd,omitempty" json:"pre_install_cmd,omitempty"`
	PostInstallCmd string `yaml:"post-install-cmd,omitempty" json:"post_install_cmd,omitempty"`
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
