package composecmd

import (
	"slices"
	"testing"
)

func TestUpArgsRemovesOrphans(t *testing.T) {
	got := upArgs("trellomcp", []string{"docker-compose.yml", "docker-compose.override.yml"})
	want := []string{
		"compose", "-p", "trellomcp",
		"-f", "docker-compose.yml", "-f", "docker-compose.override.yml",
		"up", "-d", "--remove-orphans",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("upArgs = %q, want %q", got, want)
	}
}
