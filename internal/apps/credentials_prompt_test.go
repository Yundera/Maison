package apps

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/yundera/maison/internal/xcomposeapp"
)

// prompt: false keeps a credential out of the first-click dialog (it stays in
// the tile menu); absent or true keeps the old behaviour.
func TestCredentialPrompt(t *testing.T) {
	for name, tc := range map[string]struct {
		yaml string
		want bool
	}{
		"absent":      {"credentials: [{username: admin, password: x}]", true},
		"true":        {"credentials: [{username: admin, password: x, prompt: true}]", true},
		"false":       {"credentials: [{username: admin, password: x, prompt: false}]", false},
		"one of two":  {"credentials: [{username: a, password: x, prompt: false}, {username: b, password: y}]", true},
		"empty entry": {"credentials: [{username: a, password: x, prompt: false}, {username: \"\"}]", false},
		"none at all": {"title: {en_us: x}", false},
	} {
		var ca xcomposeapp.App
		if err := yaml.Unmarshal([]byte(tc.yaml), &ca); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := promptsCredentials(&ca); got != tc.want {
			t.Errorf("%s: promptsCredentials = %v, want %v", name, got, tc.want)
		}
	}
	if promptsCredentials(nil) {
		t.Error("nil app prompts")
	}
}
