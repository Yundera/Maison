package xcomposeapp

import (
	"reflect"
	"strings"
	"testing"
)

func TestDisclosureParse(t *testing.T) {
	a := parseBlock(t, `
title: {en_us: Foundry}
cost: paid
audience: technical
accounts:
  - provider: foundryvtt.com
    url: https://foundryvtt.com/
inputs:
  - env: FOUNDRY_USERNAME
    label: {en_us: foundryvtt.com username}
    required: true
  - env: FOUNDRY_PASSWORD
    label: {en_us: foundryvtt.com password}
    description: {en_us: Only used to download the release.}
    secret: true
    required: true
`)
	if len(a.Problems) != 0 {
		t.Fatalf("problems: %v", a.Problems)
	}
	if a.Cost != CostPaid || !a.Technical() {
		t.Errorf("cost = %q, audience = %q", a.Cost, a.Audience)
	}
	if !reflect.DeepEqual(a.Accounts, []Account{{Provider: "foundryvtt.com", URL: "https://foundryvtt.com/"}}) {
		t.Errorf("accounts = %+v", a.Accounts)
	}
	if len(a.Inputs) != 2 {
		t.Fatalf("inputs = %+v", a.Inputs)
	}
	pw, ok := a.Input("FOUNDRY_PASSWORD")
	if !ok || !pw.Secret || !pw.Required || pw.Label.Value() != "foundryvtt.com password" || pw.Description.Value() == "" {
		t.Errorf("FOUNDRY_PASSWORD = %+v", pw)
	}
	if user, _ := a.Input("FOUNDRY_USERNAME"); user.Secret {
		t.Errorf("username should not be secret")
	}
}

// Absent, everything defaults to the quiet case: free, for everyone, nothing to ask.
func TestDisclosureDefaults(t *testing.T) {
	a := parseBlock(t, `title: {en_us: App}`)
	if a.Cost != "" || a.Technical() || a.Accounts != nil || a.Inputs != nil || len(a.Problems) != 0 {
		t.Errorf("defaults: %+v", a)
	}
}

// A value Maison cannot read costs that value, never the block.
func TestBadDisclosureIsDroppedNotFatal(t *testing.T) {
	a := parseBlock(t, `
title: {en_us: App}
cost: expensive
audience: wizards
accounts:
  - provider: plex.tv
    url: not a url
  - url: https://example.com
inputs:
  - env: 1BAD
    label: {en_us: Bad}
  - env: TOKEN
    label: Token
    secret: true
  - env: TOKEN
    label: {en_us: Again}
`)
	if a.Title.Value() != "App" {
		t.Errorf("title lost: %v", a.Title)
	}
	if a.Cost != "" || a.Audience != "" {
		t.Errorf("cost = %q, audience = %q", a.Cost, a.Audience)
	}
	if !reflect.DeepEqual(a.Accounts, []Account{{Provider: "plex.tv"}}) {
		t.Errorf("accounts = %+v", a.Accounts)
	}
	// TOKEN is kept even though its label was refused: an unlabelled prompt beats none.
	if len(a.Inputs) != 1 || a.Inputs[0].Env != "TOKEN" || !a.Inputs[0].Secret || a.Inputs[0].Label != nil {
		t.Errorf("inputs = %+v", a.Inputs)
	}
	got := strings.Join(a.Problems, "\n")
	for _, w := range []string{
		"cost: unknown value expensive", "audience: unknown value wizards",
		"accounts[0].url", "accounts[1]: provider is required",
		`inputs[0].env: "1BAD"`, "inputs[1].label: must be a locale map", "inputs[1].label: required",
		"inputs[2].env: TOKEN is declared twice",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing problem %q in:\n%s", w, got)
		}
	}
}

func TestDisclosureNotALists(t *testing.T) {
	a := parseBlock(t, "accounts: plex.tv\ninputs: API_KEY")
	if a.Accounts != nil || a.Inputs != nil || len(a.Problems) != 2 {
		t.Errorf("accounts = %v, inputs = %v, problems = %v", a.Accounts, a.Inputs, a.Problems)
	}
}
