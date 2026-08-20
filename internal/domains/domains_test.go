package domains

import (
	"encoding/json"
	"testing"
)

// A settings.json written before the field was renamed still carries `directives`.
// Dropping it on upgrade would take the deployment's TLS configuration with it —
// the sslip/nip distinction lives entirely in these keys — so the old name is read
// for as long as files on disk might still use it.
func TestDomainReadsTheLegacyDirectivesKey(t *testing.T) {
	var d Domain
	if err := json.Unmarshal([]byte(`{
		"name": "nip",
		"domain": "${APP_PUBLIC_IP_DASH}.nip.io",
		"directives": {"import": "gateway_tls"}
	}`), &d); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if d.Labels["import"] != "gateway_tls" {
		t.Fatalf("labels = %v, want the legacy directives carried over", d.Labels)
	}
}

// The new name wins where both appear, so a file half-rewritten by hand does not
// silently keep the stale half.
func TestDomainPrefersLabelsOverDirectives(t *testing.T) {
	var d Domain
	if err := json.Unmarshal([]byte(`{
		"name": "nip",
		"domain": "n.example.com",
		"labels": {"import": "new"},
		"directives": {"import": "old"}
	}`), &d); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if d.Labels["import"] != "new" {
		t.Fatalf("labels = %v, want the labels key to win", d.Labels)
	}
}

// Only the new name is written back, so a file converges on it after one save.
func TestDomainWritesOnlyLabels(t *testing.T) {
	b, err := json.Marshal(Domain{Name: "nip", Domain: "n.example.com",
		Labels: map[string]string{"import": "gateway_tls"}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := back["directives"]; ok {
		t.Errorf("directives written back in %s", b)
	}
	if _, ok := back["labels"]; !ok {
		t.Errorf("no labels in %s", b)
	}
}

// A domain with no labels of its own round-trips as one — sslip.io carries none,
// and an empty map is not the same as an absent one to whoever reads the file.
func TestDomainWithoutLabelsRoundTrips(t *testing.T) {
	b, err := json.Marshal(Domain{Name: "sslip", Domain: "${APP_PUBLIC_IP_DASH}.sslip.io"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var d Domain
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if d.Labels != nil {
		t.Errorf("labels = %v, want none", d.Labels)
	}
	if d.Name != "sslip" || d.Domain != "${APP_PUBLIC_IP_DASH}.sslip.io" {
		t.Errorf("round-trip lost the entry: %+v", d)
	}
}
