package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLint(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	clean := write("clean.yml", "services: {app: {image: a:1}}\nx-compose-app:\n  title: {en_us: App}\n  tips: {en_us: Hi, fr_fr: Salut}\n")
	plain := write("plain.yml", "services: {app: {image: a:1}}\nx-compose-app:\n  title: App\n  credentials: [{label: {fr_fr: Admin}, username: a}]\n")
	blocked := write("blocked.yml", "services: {}\nx-compose-app:\n  title: {en_us: App}\n  data_version: 2\n  migrations: [{id: 2, blocked: by hand}]\n")

	var out, errOut bytes.Buffer
	if code := runCLI([]string{"lint", clean}, nil, &out, &errOut); code != 0 || out.Len() != 0 {
		t.Fatalf("clean file: code %d, out %q, err %q", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := runCLI([]string{"lint", clean, plain, blocked}, nil, &out, &errOut); code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	for _, want := range []string{
		plain + ": title: must be a locale map",
		plain + ": credentials[0].label: must be a locale map",
		blocked + ": x-compose-app migrations[0].blocked: must be a locale map",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), clean) {
		t.Errorf("clean file reported:\n%s", out.String())
	}
	if code := runCLI([]string{"lint"}, nil, &out, &errOut); code != 2 {
		t.Errorf("no files: code = %d, want 2", code)
	}
}

// Every script entry is checked the way it would fail on a box — including a
// `file:` the store app does not ship, and a credential change Maison would
// silently drop.
func TestLintScriptEntries(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	os.WriteFile(filepath.Join(dir, "scripts", "there.sh"), []byte("true\n"), 0o644)
	path := filepath.Join(dir, "docker-compose.yml")
	os.WriteFile(path, []byte(`services: {app: {image: a:1}}
x-compose-app:
  title: {en_us: App}
  steps:
    - {name: ok, at: setup, file: scripts/there.sh}
    - {name: gone, at: started, file: scripts/gone.sh}
    - {name: when, at: pre_up, script: "true"}
    - {name: ok, at: setup, script: "true"}
  credentials:
    - {label: {en_us: A}, password: "${P}", change: {apply: hook, hook: "echo old"}}
    - {label: {en_us: B}, password: "${P}", change: {apply: script, file: scripts/passwd.sh}}
  data_version: 2
  migrations:
    - {id: 2, file: scripts/0002.sh}
`), 0o644)
	var out, errOut bytes.Buffer
	if code := runCLI([]string{"lint", path}, nil, &out, &errOut); code != 1 {
		t.Fatalf("code = %d, want 1:\n%s", code, out.String())
	}
	for _, want := range []string{
		"step gone: file scripts/gone.sh is not in the app's folder",
		`step when: at "pre_up"`,
		"step ok: name used twice",
		"credentials[0].change cannot be used",
		"credentials[1].change: file scripts/passwd.sh is not in the app's folder",
		"migration 2: file scripts/0002.sh is not in the app's folder",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "there.sh") {
		t.Errorf("a shipped file was reported:\n%s", out.String())
	}
}

// A route Maison would skip on a box is reported in the store, and the first route
// — the web UI — must be on a port its service exposes.
func TestLintRoutes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "routes.yml")
	body := `services:
  web: {image: a:1, expose: ["8080"]}
  db: {image: b:1}
x-compose-app:
  title: {en_us: App}
  routes:
    - {service: web, upstream-port: 80}
    - {service: nope, name: Admin_UI, upstream-port: 99999, insecure-upstream: true}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := runCLI([]string{"lint", path}, nil, &out, &errOut); code != 1 {
		t.Fatalf("code = %d, want 1:\n%s", code, out.String())
	}
	for _, want := range []string{
		"must be a lowercase DNS label",
		`service "nope" is not in the compose`,
		"is not a port",
		"insecure-upstream only applies",
		`which service "web" does not expose`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// A published host port means a listener the internet must dial, which a box behind
// NAT or the tunnel does not have: the app has to say so with requires. A port bound
// to one named address is not for the internet.
func TestLintPublishedPortsNeedPublicIP(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	head := "x-compose-app:\n  title: {en_us: App}\n"
	short := write("short.yml", "services: {wg: {image: a:1, ports: [\"51820:51820/udp\"]}}\n"+head)
	long := write("long.yml", "services: {db: {image: a:1, ports: [{target: 6881, published: 6881, protocol: tcp}]}}\n"+head)
	declared := write("declared.yml", "services: {wg: {image: a:1, ports: [\"51820:51820/udp\"]}}\n"+head+"  requires: [public-ip]\n")
	bound := write("bound.yml", "services: {smb: {image: a:1, ports: [\"10.9.0.1:445:445\", {target: 139, published: 139, host_ip: 10.9.0.1}, \"8080\"]}}\n"+head)
	casaos := write("casaos.yml", "services: {wg: {image: a:1, ports: [\"51820:51820/udp\"]}}\nx-casaos:\n  title: {en_us: App}\n  tags: [needs-public-ip]\n")

	var out, errOut bytes.Buffer
	runCLI([]string{"lint", short, long, declared, bound, casaos}, nil, &out, &errOut)
	for _, want := range []string{
		short + ": service wg publishes 51820:51820/udp on every address",
		long + ": service db publishes 6881/tcp on every address",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	for _, clean := range []string{declared, bound, casaos} {
		if strings.Contains(out.String(), clean+":") {
			t.Errorf("%s reported:\n%s", clean, out.String())
		}
	}
}

func TestLintRequirements(t *testing.T) {
	p := filepath.Join(t.TempDir(), "docker-compose.yml")
	os.WriteFile(p, []byte("services: {app: {image: a:1}}\nx-compose-app:\n  title: {en_us: App}\n  requires: [gpu]\n  resources: {min: {memory: 4}}\n"), 0o644)
	var out, errOut bytes.Buffer
	if code := runCLI([]string{"lint", p}, nil, &out, &errOut); code != 1 {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{"requires[0]: unknown capability gpu", "resources.min.memory: must be a size"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
