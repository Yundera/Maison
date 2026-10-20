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
