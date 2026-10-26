package xcomposeapp

import (
	"reflect"
	"strings"
	"testing"
)

func TestRequirementsParse(t *testing.T) {
	a := parseBlock(t, `
title: {en_us: App}
requires: [public-ip, gpu-nvidia]
resources:
  min: {cpu: 2, memory: 2G, disk: 10G}
  recommended: {cpu: 4.5, memory: 1.5GiB}
`)
	if len(a.Problems) != 0 {
		t.Fatalf("problems: %v", a.Problems)
	}
	if !reflect.DeepEqual(a.Requires, []string{"public-ip", "gpu-nvidia"}) {
		t.Errorf("requires = %v", a.Requires)
	}
	want := &Resources{
		Min:         &ResourceSet{CPU: 2, Memory: 2 << 30, Disk: 10 << 30},
		Recommended: &ResourceSet{CPU: 4.5, Memory: 3 << 29},
	}
	if !reflect.DeepEqual(a.Resources, want) {
		t.Errorf("resources = %+v %+v", a.Resources.Min, a.Resources.Recommended)
	}
}

// A requirement Maison cannot read costs that requirement, never the block.
func TestBadRequirementsAreDroppedNotFatal(t *testing.T) {
	a := parseBlock(t, `
title: {en_us: App}
requires: [public-ip, tpu, 3]
resources:
  min: {cpu: lots, memory: 2, disk: 5G, swap: 1G}
  maximum: {cpu: 8}
`)
	if a.Title.Value() != "App" {
		t.Errorf("title lost: %v", a.Title)
	}
	if !reflect.DeepEqual(a.Requires, []string{"public-ip"}) {
		t.Errorf("requires = %v", a.Requires)
	}
	if a.Resources == nil || a.Resources.Min == nil || a.Resources.Min.Disk != 5<<30 || a.Resources.Min.CPU != 0 || a.Resources.Min.Memory != 0 {
		t.Errorf("resources.min = %+v", a.Resources)
	}
	got := strings.Join(a.Problems, "\n")
	for _, w := range []string{"requires[1]: unknown capability tpu", "requires[2]", "resources.min.cpu", "resources.min.memory: must be a size", "resources.min.swap", "resources.maximum"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing problem %q in:\n%s", w, got)
		}
	}
}

func TestRequiresNotAList(t *testing.T) {
	a := parseBlock(t, "requires: public-ip")
	if a.Requires != nil || len(a.Problems) != 1 {
		t.Errorf("requires=%v problems=%v", a.Requires, a.Problems)
	}
}

func TestMinMemoryFoldsIntoResources(t *testing.T) {
	a := parseBlock(t, "min_memory: 512")
	if a.Resources == nil || a.Resources.Min.Memory != 512<<20 {
		t.Fatalf("resources = %+v", a.Resources)
	}
	a = parseBlock(t, "min_memory: 512\nresources: {min: {memory: 1G}}")
	if a.Resources.Min.Memory != 1<<30 {
		t.Errorf("explicit min.memory overridden: %d", a.Resources.Min.Memory)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"512M": 512 << 20, "2G": 2 << 30, "2g": 2 << 30, "1.5GiB": 3 << 29, "1T": 1 << 40, "64Mb": 64 << 20} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "2", "G", "-1G", "2X"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) accepted", in)
		}
	}
	for b, want := range map[int64]string{2 << 30: "2G", 3 << 29: "1.5G", 512 << 20: "512M"} {
		if got := FormatSize(b); got != want {
			t.Errorf("FormatSize(%d) = %s, want %s", b, got, want)
		}
	}
}
