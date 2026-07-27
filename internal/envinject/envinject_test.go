package envinject

import (
	"testing"

	"github.com/yundera/maison/internal/config"
)

// A deployment's host path normally ends in /DATA, which is exactly what makes
// naive sequential replacement double-rewrite an expanded ${DATA_ROOT}.
func TestRewriteToHostPath(t *testing.T) {
	cfg := config.Config{DataRoot: "/DATA", DataHostPath: "/opt/maison/DATA"}
	cases := []struct{ in, want string }{
		{"mkdir -p ${DATA_ROOT}/AppData/x", "mkdir -p /opt/maison/DATA/AppData/x"},
		{"mkdir -p $DATA_ROOT/AppData/x", "mkdir -p /opt/maison/DATA/AppData/x"},
		{"docker run -v /DATA/Media:/media img", "docker run -v /opt/maison/DATA/Media:/media img"},
		{"echo ${DATA_ROOT} /DATA", "echo /opt/maison/DATA /opt/maison/DATA"},
		{"echo nothing here", "echo nothing here"},
	}
	for _, c := range cases {
		if got := RewriteToHostPath(c.in, cfg); got != c.want {
			t.Errorf("RewriteToHostPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// When the container mount point and the host path agree there is nothing to do.
	same := config.Config{DataRoot: "/DATA", DataHostPath: "/DATA"}
	if got := RewriteToHostPath("cp x ${DATA_ROOT}/y", same); got != "cp x ${DATA_ROOT}/y" {
		t.Errorf("identity deployment: got %q", got)
	}
}

// ContainerPath and HostPath are inverses across the data mount.
func TestPathMapping(t *testing.T) {
	cfg := config.Config{DataRoot: "/DATA", DataHostPath: "/opt/maison/DATA"}
	for _, in := range []string{
		"/DATA/AppData/jellyfin/config",
		"${DATA_ROOT}/AppData/jellyfin/config",
		"/opt/maison/DATA/AppData/jellyfin/config",
	} {
		const wantContainer = "/DATA/AppData/jellyfin/config"
		got := ContainerPath(in, cfg)
		if got != wantContainer {
			t.Fatalf("ContainerPath(%q) = %q, want %q", in, got, wantContainer)
		}
		const wantHost = "/opt/maison/DATA/AppData/jellyfin/config"
		if back := HostPath(got, cfg); back != wantHost {
			t.Fatalf("HostPath(%q) = %q, want %q", got, back, wantHost)
		}
	}
	// A path outside the data mount is left alone — the caller decides what to do.
	if got := HostPath("/etc/hosts", cfg); got != "/etc/hosts" {
		t.Fatalf("HostPath(/etc/hosts) = %q", got)
	}
}
