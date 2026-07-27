// Package composefile is a light reader for docker-compose.yml files — just
// enough to extract the x-casaos metadata and service names without pulling in
// the full compose engine (that lands in the install path in a later milestone).
package composefile

import (
	"os"

	"gopkg.in/yaml.v3"

	"github.com/yundera/maison/internal/xcasaos"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// File is a minimal view of a compose project file.
type File struct {
	Name        string             `yaml:"name,omitempty"`
	Services    map[string]Service `yaml:"services"`
	XCasaOS     map[string]any     `yaml:"x-casaos"`
	XComposeApp map[string]any     `yaml:"x-compose-app"`
}

// Service is a minimal view of one compose service.
type Service struct {
	Image string `yaml:"image,omitempty"`
}

// Load parses a compose file from disk.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse parses compose YAML bytes.
func Parse(b []byte) (*File, error) {
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// StoreInfo returns the parsed x-casaos metadata, or an error if absent.
func (f *File) StoreInfo() (*xcasaos.StoreInfo, error) {
	return xcasaos.Parse(f.XCasaOS)
}

// ComposeApp returns the parsed x-compose-app metadata, or an error if absent.
func (f *File) ComposeApp() (*xcomposeapp.App, error) {
	return xcomposeapp.Parse(f.XComposeApp)
}
