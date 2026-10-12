package apps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/config"
)

// Docker reports an unmanaged stack's working dir as the HOST spells it. When the
// data folder is mounted at a different path inside this container, metaFor must
// open it through the mount, or the stack's metadata — and with it its lifecycle
// guard — silently disappears.
func TestMetaForRemapsHostWorkingDir(t *testing.T) {
	dataRoot := t.TempDir()
	cfg := config.Config{DataRoot: dataRoot, DataHostPath: "/srv/host-data"}
	r := New(cfg, nil)

	// An unmanaged stack outside AppData, so the managed lookup cannot find it.
	stack := filepath.Join(dataRoot, "stacks", "mesh")
	if err := os.MkdirAll(stack, 0o755); err != nil {
		t.Fatal(err)
	}
	compose := "services:\n  web:\n    image: x\nx-compose-app:\n  view: system\n"
	if err := os.WriteFile(filepath.Join(stack, "docker-compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}

	_, ca := r.metaFor("mesh", "/srv/host-data/stacks/mesh")
	if ca == nil {
		t.Fatal("metaFor found no x-compose-app at the host working dir; it was opened without remapping to DATA_ROOT")
	}
}
