package composecmd

import (
	"slices"
	"strings"
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

func TestImagesFromConfig(t *testing.T) {
	raw := []byte(`{"services":{
		"app":{"image":"reg.example/app:1.2"},
		"side":{"image":"reg.example/app:1.2"},
		"db":{"image":"postgres:16","pull_policy":"missing"},
		"built":{"image":"local/built:dev","build":{"context":"."}},
		"local":{"image":"local/only:1","pull_policy":"never"},
		"noimg":{"build":{"context":"."}}
	}}`)
	got, err := imagesFromConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"postgres:16", "reg.example/app:1.2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("images = %v, want %v", got, want)
	}
}
