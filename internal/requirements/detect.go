package requirements

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/yundera/maison/internal/xcomposeapp"
)

// The capability probes. Each is a pure function of what the caller gathered, so
// the gathering (host /proc, the Docker daemon, sysfs) stays with the server and
// the judgement stays testable.

// DetectPublicIP judges whether inbound connections can reach the box directly.
//
// The deployment writes the box's public address into the app environment
// (APP_PUBLIC_IPV4 / APP_PUBLIC_IPV6, or APP_PUBLIC_IP). If the HOST carries that
// address on an interface, nothing translates in front of it: a VPS, or a box with
// a routed public IP. If it does not, the address belongs to something upstream —
// a home router doing NAT, a CGNAT, the mesh tunnel's exit — and a raw TCP/UDP
// listener on the box is not reachable from it unless someone forwarded the port,
// which is exactly what the operator's `yes` override is for.
//
// hostAddrs/ok are system.HostAddresses(); ok false (no host /proc) is Unknown.
func DetectPublicIP(env map[string]string, hostAddrs map[string]bool, ok bool) Probe {
	var pub []string
	for _, k := range []string{"APP_PUBLIC_IPV4", "APP_PUBLIC_IPV6", "APP_PUBLIC_IP"} {
		if v := strings.TrimSpace(env[k]); v != "" {
			pub = append(pub, v)
		}
	}
	if len(pub) == 0 {
		return Probe{Value: Unknown, Source: "the deployment states no public address (APP_PUBLIC_IP)"}
	}
	if !ok {
		return Probe{Value: Unknown, Source: "the host's addresses are not readable (HOST_PROC)"}
	}
	for _, a := range pub {
		if hostAddrs[a] {
			return Probe{Value: Yes, Source: "public address " + a + " is on a host interface"}
		}
	}
	return Probe{Value: No, Source: "public address " + pub[0] + " is not on any host interface (NAT or tunnel)"}
}

// DetectNvidia judges whether containers can be given an NVIDIA GPU: the Docker
// daemon has the nvidia runtime (nvidia-container-toolkit) registered. err is the
// daemon query's; a daemon that cannot be asked is Unknown.
func DetectNvidia(runtimes []string, err error) Probe {
	if err != nil {
		return Probe{Value: Unknown, Source: "the Docker daemon could not be queried"}
	}
	for _, r := range runtimes {
		if r == "nvidia" {
			return Probe{Value: Yes, Source: "Docker has the nvidia runtime"}
		}
	}
	return Probe{Value: No, Source: "Docker has no nvidia runtime"}
}

// DetectDRI judges whether the host has a render node (/dev/dri/renderD*) an app
// can be handed for video acceleration. sysfs is not namespaced for DRM, so the
// container's /sys/class/drm lists the host's devices. sysRoot is "/sys" outside
// tests.
func DetectDRI(sysRoot string) Probe {
	dir := filepath.Join(sysRoot, "class", "drm")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return Probe{Value: No, Source: "no DRM devices on the host"}
		}
		return Probe{Value: Unknown, Source: dir + " is not readable"}
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "renderD") {
			return Probe{Value: Yes, Source: "render node " + e.Name()}
		}
	}
	return Probe{Value: No, Source: "no render node on the host"}
}

// Probes assembles the three capability probes by name.
func Probes(publicIP, nvidia, dri Probe) map[string]Probe {
	return map[string]Probe{
		xcomposeapp.CapPublicIP:  publicIP,
		xcomposeapp.CapGPUNvidia: nvidia,
		xcomposeapp.CapGPUDri:    dri,
	}
}
