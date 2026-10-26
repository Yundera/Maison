package xcasaos

import (
	"reflect"
	"testing"
)

func TestTipsTranslateToALocaleMap(t *testing.T) {
	cases := []struct {
		name string
		tips Tips
		want map[string]string
	}{
		{"guidance + custom", Tips{BeforeInstall: map[string]string{"en_us": "Read me", "fr_fr": "Lisez-moi"}, Custom: "Port 8080"},
			map[string]string{"en_us": "Read me\n\nPort 8080", "fr_fr": "Lisez-moi\n\nPort 8080"}},
		{"custom alone becomes en_us", Tips{Custom: "Port 8080"}, map[string]string{"en_us": "Port 8080"}},
		{"guidance without en_us gets custom as en_us", Tips{BeforeInstall: map[string]string{"fr_fr": "Lisez-moi"}, Custom: "Port 8080"},
			map[string]string{"en_us": "Port 8080", "fr_fr": "Lisez-moi\n\nPort 8080"}},
		{"nothing", Tips{}, map[string]string{}},
	}
	for _, c := range cases {
		if got := (&StoreInfo{Tips: c.tips}).tips(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The CasaOS needs-public-ip tag is the translated block's `requires`; other
// tags are topical and translate to nothing.
func TestNeedsPublicIPTagTranslatesToRequires(t *testing.T) {
	m := (&StoreInfo{Tags: []string{"games", "needs-public-ip"}}).ComposeApp()
	if !reflect.DeepEqual(m["requires"], []any{"public-ip"}) {
		t.Errorf("requires = %v", m["requires"])
	}
	if m := (&StoreInfo{Tags: []string{"games"}}).ComposeApp(); m["requires"] != nil {
		t.Errorf("topical tag translated: %v", m["requires"])
	}
}
