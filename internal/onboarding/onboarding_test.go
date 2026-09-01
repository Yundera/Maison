package onboarding

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/config"
)

// write puts contents at the onboarding path of a scratch deployment and returns
// its config. No contents means no file — the normal "off" state.
func write(t *testing.T, contents string) config.Config {
	t.Helper()
	cfg := config.Config{DataRoot: t.TempDir()}
	if err := os.MkdirAll(cfg.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if contents != "" {
		if err := os.WriteFile(Path(cfg), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

// TestPresenceIsTheState pins the whole contract: the file being there is what
// arms the gate, and deleting it is what clears it. Nothing else is consulted,
// and nothing is remembered between calls — which is what lets a deployment's
// self-check reconcile the file against the truth and have Maison follow on the
// very next request.
func TestPresenceIsTheState(t *testing.T) {
	cfg := write(t, `{"url":"https://admin.box.example/"}`)

	if url, pending := Pending(cfg); !pending || url != "https://admin.box.example/" {
		t.Fatalf("with the file present: got (%q, %v), want the URL and true", url, pending)
	}

	if err := os.Remove(Path(cfg)); err != nil {
		t.Fatal(err)
	}
	if url, pending := Pending(cfg); pending {
		t.Errorf("after the deployment deleted the file: got (%q, true), want not pending", url)
	}
}

// TestFailsOpen guards the asymmetry documented on Pending. Every one of these
// is a broken or absent config, and answering "pending" to any of them would
// strand the owner on an interstitial whose only button goes nowhere — with no
// way back to the dashboard, since the gate has no skip.
func TestFailsOpen(t *testing.T) {
	for _, c := range []struct{ name, file string }{
		{"no file at all", ""},
		{"not json", "this is not json\n"},
		{"empty object", `{}`},
		{"empty url", `{"url":"   "}`},
		{"relative url", `{"url":"/setup"}`},
		{"no host", `{"url":"https://"}`},
		// A typo in a root-written config file must not become script running on
		// the dashboard's own origin.
		{"script scheme", `{"url":"javascript:alert(1)"}`},
		{"data scheme", `{"url":"data:text/html,<script>alert(1)</script>"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if url, pending := Pending(write(t, c.file)); pending {
				t.Errorf("got (%q, true), want not pending", url)
			}
		})
	}
}

// TestPathIsBesideTheOtherDeploymentFiles pins where a deployment must write it.
// It has to be Maison's state directory and not the stack directory: on a PCS
// those are the same folder, but only the state half survives the self-check that
// redeploys docker-compose.yml and .env over the top of it.
func TestPathIsBesideTheOtherDeploymentFiles(t *testing.T) {
	cfg := config.Config{DataRoot: "/DATA"}
	want := filepath.Join("/DATA", "AppData", "maison", "onboarding.json")
	if got := Path(cfg); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}
