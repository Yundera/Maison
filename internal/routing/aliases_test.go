package routing

import (
	"strings"
	"testing"

	"github.com/yundera/maison/internal/domains"
)

var aliasSetting = Setting{Renderer: Labels, Domains: []domains.Domain{
	{Name: "gateway", Domain: "{name}-${APP_DOMAIN}", Labels: map[string]string{"import": "gateway_tls"}},
	{Name: "dot", Domain: "{name}.${APP_DOMAIN}"},
}}

var aliasEnv = EnvResolver(map[string]string{"APP_DOMAIN": "box.example.com"})

var aliasRoutes = []Route{
	{Name: "outline", Service: "outline", Port: 80, Scheme: "http", Path: "/", WebUI: true},
	{Name: "outline-auth", Service: "outline-dex", Port: 5556, Scheme: "http", Path: "/", WebUI: true},
}

func TestPrimaryAliasTakesOverTheRoutesAddress(t *testing.T) {
	entries := Table(aliasRoutes, aliasSetting, aliasEnv, nil)
	all, errs := WithAliases(entries, aliasRoutes, []Alias{{Name: "wiki", Domains: []string{"dot"}, Primary: true}}, aliasSetting, aliasEnv, nil)
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	vars := Vars(all, aliasSetting)
	if vars["APP_URL"] != "https://wiki.box.example.com" {
		t.Errorf("APP_URL = %q", vars["APP_URL"])
	}
	if !strings.HasPrefix(vars["APP_URLS"], "https://wiki.box.example.com,") {
		t.Errorf("APP_URLS = %q, want the alias first", vars["APP_URLS"])
	}
	if vars["ROUTE_OUTLINE_AUTH_URL"] != "https://outline-auth-box.example.com" {
		t.Errorf("the other route moved: %q", vars["ROUTE_OUTLINE_AUTH_URL"])
	}
	// The alias is not a `<name>-` host: the gate must not learn a suffix from it.
	if strings.Contains(vars["APP_HOST_SUFFIXES"], "wiki") {
		t.Errorf("APP_HOST_SUFFIXES = %q", vars["APP_HOST_SUFFIXES"])
	}
	if got := WebURL(aliasRoutes, []Alias{{Name: "wiki", Domains: []string{"dot"}, Primary: true}}, aliasSetting, aliasEnv); got != "https://wiki.box.example.com/" {
		t.Errorf("WebURL = %q", got)
	}
	primaries := 0
	for _, e := range all {
		if e.Primary && e.Name == "outline" {
			primaries++
		}
	}
	if primaries != 1 {
		t.Errorf("route outline has %d primary entries, want 1", primaries)
	}
}

func TestAliasDefaultsToEveryDomainAndCarriesItsLabels(t *testing.T) {
	all, errs := WithAliases(Table(aliasRoutes, aliasSetting, aliasEnv, nil), aliasRoutes, []Alias{{Name: "auth", Route: "outline-auth"}}, aliasSetting, aliasEnv, nil)
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	var got []string
	for _, e := range all {
		if e.Alias == "auth" {
			if e.Service != "outline-dex" || e.Port != 5556 {
				t.Errorf("alias entry not rendered as its route: %+v", e)
			}
			got = append(got, e.Host+"|"+e.Labels["import"])
		}
	}
	if strings.Join(got, ",") != "auth-${APP_DOMAIN}|gateway_tls,auth.${APP_DOMAIN}|" {
		t.Errorf("alias entries = %v", got)
	}
}

func TestAliasRefusals(t *testing.T) {
	entries := Table(aliasRoutes, aliasSetting, aliasEnv, nil)
	cases := []struct {
		alias Alias
		want  string
	}{
		{Alias{Name: "Not_A_Label"}, "DNS label"},
		{Alias{Name: "x", Route: "nope"}, "no route"},
		{Alias{Name: "x", Domains: []string{"lan"}}, "no domain"},
		{Alias{Name: "outline", Domains: []string{"dot"}}, "own names"}, // outline.<box> is the route's own dot host
		{Alias{Name: "taken", Domains: []string{"dot"}}, "other's"},
	}
	claimed := map[string]string{"taken.box.example.com": "other"}
	for _, c := range cases {
		all, errs := WithAliases(entries, aliasRoutes, []Alias{c.alias}, aliasSetting, aliasEnv, claimed)
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), c.want) {
			t.Errorf("%+v: errs = %v, want %q", c.alias, errs, c.want)
		}
		for _, e := range all {
			if e.Alias != "" {
				t.Errorf("%+v: published %s anyway", c.alias, e.Host)
			}
		}
	}
	if _, errs := WithAliases(nil, aliasRoutes, []Alias{{Name: "x"}}, Setting{Renderer: Ports}, aliasEnv, nil); len(errs) != 1 {
		t.Errorf("ports renderer: errs = %v", errs)
	}
}

func TestSecondPrimaryAliasIsAnExtraName(t *testing.T) {
	al := []Alias{{Name: "a", Domains: []string{"dot"}, Primary: true}, {Name: "b", Domains: []string{"dot"}, Primary: true}}
	all, errs := WithAliases(Table(aliasRoutes, aliasSetting, aliasEnv, nil), aliasRoutes, al, aliasSetting, aliasEnv, nil)
	if len(errs) != 1 {
		t.Fatalf("errs = %v", errs)
	}
	if Vars(all, aliasSetting)["APP_URL"] != "https://a.box.example.com" {
		t.Errorf("first primary alias lost")
	}
	if !strings.Contains(Vars(all, aliasSetting)["APP_URLS"], "https://b.box.example.com") {
		t.Errorf("second alias not published as an extra name")
	}
}
