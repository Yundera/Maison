package adapter

import (
	"context"
	"errors"
	"testing"

	"github.com/yundera/maison/internal/config"
)

// An approval names a repository, so the tag an update moves to and the digest a
// deployment pins are both covered by it — while a registry port is not mistaken for one.
func TestImageRepoDropsTagAndDigestOnly(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io/yundera/maison-kopia-engine:1.3.0":            "ghcr.io/yundera/maison-kopia-engine",
		"ghcr.io/yundera/maison-kopia-engine":                  "ghcr.io/yundera/maison-kopia-engine",
		"ghcr.io/yundera/maison-kopia-engine@sha256:abc":       "ghcr.io/yundera/maison-kopia-engine",
		"ghcr.io/yundera/maison-kopia-engine:1.3.0@sha256:abc": "ghcr.io/yundera/maison-kopia-engine",
		"registry.local:5000/kopia":                            "registry.local:5000/kopia",
		"registry.local:5000/kopia:2":                          "registry.local:5000/kopia",
		"kopia:latest":                                         "kopia",
	} {
		if got := ImageRepo(in); got != want {
			t.Errorf("ImageRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDescriptorEqualComparesTheSecretByValue(t *testing.T) {
	a := Descriptor{EngineID: "kopia", Image: "i:1", Secret: &SecretDescriptor{File: "repository.password", Escrow: true}}
	b := a
	b.Secret = &SecretDescriptor{File: "repository.password", Escrow: true}
	if !a.Equal(b) {
		t.Error("identical descriptors with distinct secret pointers compare unequal")
	}
	b.Image = "i:2"
	if a.Equal(b) {
		t.Error("a changed image compares equal")
	}
	c := a
	c.Secret = nil
	if a.Equal(c) {
		t.Error("a dropped secret block compares equal")
	}
}

// An engine nobody approved runs nothing — not even the capabilities probe. The runner
// points at a docker binary that does not exist, so any call that got as far as running
// something would fail with a different error.
func TestAnUnapprovedEngineRunsNothing(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	approved := false
	p := NewGated(cfg, Descriptor{EngineID: "kopia", Image: "i:1", Container: "kopia-engine", Hostname: "h"},
		func() bool { return approved })
	p.runner.Docker = "/nonexistent/docker"

	if err := p.call(context.Background(), capsTimeout, nil, nil, "capabilities"); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("call = %v, want ErrNotApproved", err)
	}
	if c := p.Caps(); c.Offsite || c.Recover {
		t.Errorf("Caps = %+v, want the zero value", c)
	}
	st := p.Status(context.Background())
	if !st.NeedsApproval || st.Connected || st.Configured {
		t.Errorf("Status = %+v, want only NeedsApproval", st)
	}

	// Approval is read on every call, so it takes effect without a new provider.
	approved = true
	if err := p.call(context.Background(), capsTimeout, nil, nil, "capabilities"); errors.Is(err, ErrNotApproved) {
		t.Error("still refused after approval")
	}
	if st := p.Status(context.Background()); st.NeedsApproval {
		t.Error("status still asks for approval after it was given")
	}
}
