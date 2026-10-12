package appstore

import "testing"

func TestSameStore(t *testing.T) {
	zip := "https://github.com/Yundera/AppStore/archive/refs/heads/main.zip"
	same := []string{
		zip,
		"https://github.com/Yundera/AppStore.git",
		"https://github.com/Yundera/AppStore",
		"github.com/Yundera/AppStore",
		"https://GitHub.com/Yundera/AppStore/tree/main",
	}
	for _, s := range same {
		if !SameStore(s, zip) || !SameStore(zip, s) {
			t.Errorf("SameStore(%q, main.zip) = false, want true", s)
		}
	}
	different := []string{
		"https://github.com/Yundera/AppStoreLab/archive/refs/heads/main.zip",
		"https://github.com/Yundera/AppStore/archive/refs/heads/maison.zip",
		"https://github.com/Yundera/AppStore/tree/dev",
		"https://example.com/store.zip",
	}
	for _, s := range different {
		if SameStore(s, zip) {
			t.Errorf("SameStore(%q, main.zip) = true, want false", s)
		}
	}
}
