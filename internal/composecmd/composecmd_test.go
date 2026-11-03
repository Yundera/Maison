package composecmd

import (
	"slices"
	"testing"
)

func TestDownArgsTakeNoFile(t *testing.T) {
	got := downArgs("outline")
	want := []string{"compose", "-p", "outline", "down"}
	if !slices.Equal(got, want) {
		t.Fatalf("downArgs = %q, want %q", got, want)
	}
}

func TestWithoutComposeFile(t *testing.T) {
	got := withoutComposeFile([]string{"PATH=/bin", "COMPOSE_FILE=/x/docker-compose.yml", "COMPOSE_FILEX=keep"})
	want := []string{"PATH=/bin", "COMPOSE_FILEX=keep"}
	if !slices.Equal(got, want) {
		t.Fatalf("withoutComposeFile = %q, want %q", got, want)
	}
}

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
