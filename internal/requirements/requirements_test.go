package requirements

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/xcomposeapp"
)

func host(publicIP Tri, overrides map[string]string) Host {
	return NewHost(Detected{
		Arch: "amd64", CPU: 4, Memory: 8 << 30, DiskFree: 100 << 30,
		Probes: map[string]Probe{xcomposeapp.CapPublicIP: {Value: publicIP}, xcomposeapp.CapGPUNvidia: {Value: No}},
	}, overrides)
}

func TestCheck(t *testing.T) {
	game := &xcomposeapp.App{Requires: []string{"public-ip"}, Architectures: []string{"amd64", "arm64"}}
	cases := []struct {
		name                       string
		app                        *xcomposeapp.App
		h                          Host
		hidden, ack, fits, blocked bool
	}{
		{"nothing declared", &xcomposeapp.App{}, host(No, nil), false, false, true, false},
		{"public-ip present", game, host(Yes, nil), false, false, true, false},
		{"public-ip absent", game, host(No, nil), true, true, false, false},
		{"public-ip unknown hides nothing", game, host(Unknown, nil), false, false, false, false},
		{"override yes beats detection", game, host(No, map[string]string{"public-ip": "yes"}), false, false, true, false},
		{"override no beats detection", game, host(Yes, map[string]string{"public-ip": "no"}), true, true, false, false},
		{"wrong arch", &xcomposeapp.App{Architectures: []string{"arm64"}}, host(Yes, nil), true, true, false, true},
		{"arch alias", &xcomposeapp.App{Architectures: []string{"x86_64"}}, host(Yes, nil), false, false, true, false},
		{"gpu absent", &xcomposeapp.App{Requires: []string{"gpu-nvidia"}}, host(Yes, nil), true, true, false, false},
		{"below min", &xcomposeapp.App{Resources: &xcomposeapp.Resources{Min: &xcomposeapp.ResourceSet{CPU: 8}}}, host(Yes, nil), false, true, false, false},
		{"below recommended only", &xcomposeapp.App{Resources: &xcomposeapp.Resources{Recommended: &xcomposeapp.ResourceSet{Memory: 16 << 30}}}, host(Yes, nil), false, false, false, false},
		{"memory within the kernel's reservation", &xcomposeapp.App{Resources: &xcomposeapp.Resources{Min: &xcomposeapp.ResourceSet{Memory: 8<<30 + 1<<28}}}, host(Yes, nil), false, false, true, false},
		{"disk short", &xcomposeapp.App{Resources: &xcomposeapp.Resources{Min: &xcomposeapp.ResourceSet{Disk: 200 << 30}}}, host(Yes, nil), false, true, false, false},
	}
	for _, c := range cases {
		r := Check(c.app, c.h)
		if r.Hidden() != c.hidden || r.NeedsAck() != c.ack || r.Fits() != c.fits || r.Blocked != c.blocked {
			t.Errorf("%s: hidden=%v ack=%v fits=%v blocked=%v (%+v)", c.name, r.Hidden(), r.NeedsAck(), r.Fits(), r.Blocked, r)
		}
	}
}

// A resource under min is reported once, as below min — not again as below
// recommended.
func TestBelowMinNotRepeated(t *testing.T) {
	app := &xcomposeapp.App{Resources: &xcomposeapp.Resources{
		Min:         &xcomposeapp.ResourceSet{CPU: 8},
		Recommended: &xcomposeapp.ResourceSet{CPU: 16, Memory: 32 << 30},
	}}
	r := Check(app, host(Yes, nil))
	if len(r.BelowMin) != 1 || len(r.BelowRecommended) != 1 || r.BelowRecommended[0].Code != "memory" {
		t.Errorf("%+v", r)
	}
}

// An unmeasured host quantity is never held against an app.
func TestUnmeasuredHostIsNotShort(t *testing.T) {
	app := &xcomposeapp.App{Architectures: []string{"arm64"}, Resources: &xcomposeapp.Resources{Min: &xcomposeapp.ResourceSet{CPU: 64, Memory: 1 << 40, Disk: 1 << 40}}}
	if r := Check(app, NewHost(Detected{}, nil)); !r.Fits() {
		t.Errorf("%+v", r)
	}
}

func TestDetectPublicIP(t *testing.T) {
	addrs := map[string]bool{"203.0.113.10": true, "10.0.0.2": true}
	cases := []struct {
		env  map[string]string
		ok   bool
		want Tri
	}{
		{map[string]string{"APP_PUBLIC_IPV4": "203.0.113.10"}, true, Yes},
		{map[string]string{"APP_PUBLIC_IP": "198.51.100.7"}, true, No},
		{map[string]string{"APP_PUBLIC_IPV4": "198.51.100.7", "APP_PUBLIC_IPV6": "2001:db8::1"}, true, No},
		{map[string]string{"APP_PUBLIC_IP": "203.0.113.10"}, false, Unknown},
		{map[string]string{}, true, Unknown},
	}
	for i, c := range cases {
		if got := DetectPublicIP(c.env, addrs, c.ok); got.Value != c.want {
			t.Errorf("case %d: %v, want %v", i, got, c.want)
		}
	}
}

func TestDetectNvidiaAndDRI(t *testing.T) {
	if DetectNvidia([]string{"runc", "nvidia"}, nil).Value != Yes || DetectNvidia([]string{"runc"}, nil).Value != No || DetectNvidia(nil, errors.New("x")).Value != Unknown {
		t.Error("nvidia")
	}
	root := t.TempDir()
	if DetectDRI(root).Value != No {
		t.Error("no class/drm should be No")
	}
	drm := filepath.Join(root, "class", "drm")
	os.MkdirAll(filepath.Join(drm, "card0"), 0o755)
	if DetectDRI(root).Value != No {
		t.Error("card without render node should be No")
	}
	os.MkdirAll(filepath.Join(drm, "renderD128"), 0o755)
	if DetectDRI(root).Value != Yes {
		t.Error("render node should be Yes")
	}
}
