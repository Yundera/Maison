package routing

import (
	"strings"
	"testing"

	"github.com/yundera/maison/internal/domains"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   Setting
		err  string // "" = valid
	}{
		{"ports default", Setting{Renderer: Ports}, ""},
		{"unknown renderer", Setting{Renderer: "nginx"}, "renderer"},
		{"labels needs a domain", Setting{Renderer: Labels}, "at least one domain"},
		{"bad bind", Setting{Renderer: Ports, Bind: "lan"}, "not an IP"},
		{"url-host with scheme", Setting{Renderer: Ports, URLHost: "http://nas"}, "url-host"},
		{"port range below 1024", Setting{Renderer: Ports, PortRange: [2]int{80, 90}}, "port-range"},
		{"unknown dialect", Setting{Renderer: Labels, Dialect: "traefik", Domains: []domains.Domain{{Name: "p", Domain: PrimaryTemplate}}}, "unknown dialect"},
		{"template without name", Setting{Renderer: External, URLTemplate: "https://apps.lan"}, "{name}"},
		{"overlapping domains", Setting{Renderer: Labels, Domains: []domains.Domain{
			{Name: "a", Domain: "{name}-${APP_DOMAIN}"},
			{Name: "b", Domain: "{name}-lan-${APP_DOMAIN}"},
		}}, "inside domain"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Normalize(c.in)
			switch {
			case c.err == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
				t.Errorf("error = %v, want one mentioning %q", err, c.err)
			}
		})
	}
}

// A host written without {name} reads as `{name}-<host>`, and is stored that way.
func TestNormalizeAddsTheNamePlaceholder(t *testing.T) {
	s, err := Normalize(Setting{Renderer: Labels, Domains: []domains.Domain{{Name: "nip", Domain: "${APP_PUBLIC_IP_DASH}.nip.io"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Domains[0].Domain; got != "{name}-${APP_PUBLIC_IP_DASH}.nip.io" {
		t.Errorf("domain = %q", got)
	}
}

// No setting: a box with a domain keeps routing as before; one without gets ports.
func TestEffectiveDefaults(t *testing.T) {
	pcs := Effective(nil, map[string]string{"APP_DOMAIN": "alice.nsl.sh"}, "")
	if pcs.Renderer != Labels || len(pcs.Domains) != 1 || pcs.Domains[0].Domain != PrimaryTemplate {
		t.Errorf("PCS default = %+v", pcs)
	}
	home := Effective(nil, nil, "192.168.1.10")
	if home.Renderer != Ports || home.Bind != DefaultBind || home.PortRange != [2]int{DefaultPortLow, DefaultPortHigh} || home.URLHost != "192.168.1.10" {
		t.Errorf("home default = %+v", home)
	}
	set := Effective(&Setting{Renderer: Ports, URLHost: "nas.lan"}, nil, "192.168.1.10")
	if set.URLHost != "nas.lan" {
		t.Errorf("the operator's url-host lost to the seen one: %+v", set)
	}
}

func TestMigrateTheOldDomainList(t *testing.T) {
	if Migrate(nil) != nil {
		t.Error("an empty list migrated to a setting")
	}
	s := Migrate([]domains.Domain{{Name: "sslip", Domain: "${APP_PUBLIC_IP_DASH}.sslip.io"}})
	if s == nil || s.Renderer != Labels || len(s.Domains) != 2 {
		t.Fatalf("migrated = %+v", s)
	}
	if s.Domains[0].Domain != PrimaryTemplate || s.Domains[1].Domain != "{name}-${APP_PUBLIC_IP_DASH}.sslip.io" {
		t.Errorf("domains = %+v", s.Domains)
	}
	if _, err := Normalize(*s); err != nil {
		t.Errorf("a migrated setting does not validate: %v", err)
	}
}

// An origin leaves out the scheme's default port, the way a browser shows it.
func TestOriginDropsTheDefaultPort(t *testing.T) {
	for _, c := range []struct {
		scheme string
		port   int
		want   string
	}{
		{"http", 80, "http://nas"},
		{"https", 443, "https://nas"},
		{"http", 8096, "http://nas:8096"},
		{"https", 80, "https://nas:80"},
	} {
		if got, _ := origin(c.scheme, "nas", c.port); got != c.want {
			t.Errorf("origin(%s, %d) = %q, want %q", c.scheme, c.port, got, c.want)
		}
	}
}

func TestWebURLAndHosts(t *testing.T) {
	s := Setting{Renderer: Labels, Domains: []domains.Domain{
		{Name: "gateway", Domain: "{name}-${APP_DOMAIN}"},
		{Name: "nip", Domain: "{name}-${APP_PUBLIC_IP_DASH}.nip.io"},
	}}
	env := map[string]string{"APP_DOMAIN": "alice.nsl.sh", "APP_PUBLIC_IP_DASH": "203-0-113-10"}
	rts := []Route{{Name: "kubo", Service: "kubo", Port: 5001, Scheme: "http", Path: "/webui/"}}
	if got := WebURL(rts, s, EnvResolver(env)); got != "https://kubo-alice.nsl.sh/webui/" {
		t.Errorf("WebURL = %q", got)
	}
	hosts := Hosts(s, "maison", EnvResolver(env))
	if strings.Join(hosts, ",") != "maison-203-0-113-10.nip.io,maison-alice.nsl.sh" {
		t.Errorf("Hosts = %v", hosts)
	}
	// An unresolved domain gives no address rather than a broken one.
	if got := WebURL(rts, s, EnvResolver(nil)); got != "" {
		t.Errorf("WebURL with no domain = %q", got)
	}
}

func TestEnvName(t *testing.T) {
	if got := EnvName("outline-auth"); got != "OUTLINE_AUTH" {
		t.Errorf("EnvName = %q", got)
	}
}
