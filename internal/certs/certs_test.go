package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var (
	now   = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	boxIP = net.ParseIP("203.0.113.10")
	cdnIP = net.ParseIP("198.51.100.7")
)

// dns: every host resolves to the box except the listed CDN-fronted ones.
func dns(fronted ...string) Lookup {
	return func(h string) ([]net.IP, error) {
		for _, f := range fronted {
			if h == f {
				return []net.IP{cdnIP}, nil
			}
		}
		if h == "nxdomain.test" {
			return nil, errors.New("no such host")
		}
		return []net.IP{boxIP}, nil
	}
}

// issue writes a certificate for names into store/<issuer>/<entry>/, the way
// Caddy lays its storage out.
func issue(t *testing.T, store, issuer, entry string, notAfter time.Time, names ...string) {
	t.Helper()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: names[0]},
		NotBefore:    now.Add(-24 * time.Hour),
		NotAfter:     notAfter,
		DNSNames:     names,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(priv)
	dir := filepath.Join(store, issuer, entry)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, entry+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(filepath.Join(dir, entry+".key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
}

const acme = "acme-v02.api.letsencrypt.org-directory"

var later = now.AddDate(0, 2, 0)

// The hosts of route "mailhub" on a Yundera-like box, in domain order.
var hosts = []string{
	"mailhub-alice.nsl.sh",          // gateway: gateway CA, fronted by the CDN
	"mailhub.alice.nsl.sh",          // dot form: wildcard once the zone is delegated
	"mailhub-203-0-113-10.sslip.io", // sslip.io: Let's Encrypt, direct
}

func TestPicksFirstDirectHostWithATrustedCertificate(t *testing.T) {
	store := t.TempDir()
	issue(t, store, acme, "mailhub-203-0-113-10.sslip.io", later, "mailhub-203-0-113-10.sslip.io")
	r, err := Pick(store, hosts, "", []net.IP{boxIP}, dns("mailhub-alice.nsl.sh", "mailhub.alice.nsl.sh"), now)
	if err != nil || r.Host != "mailhub-203-0-113-10.sslip.io" {
		t.Fatalf("got %q, %v", r.Host, err)
	}
}

func TestWildcardCoversTheDotForm(t *testing.T) {
	store := t.TempDir()
	issue(t, store, acme, "wildcard_.alice.nsl.sh", later, "*.alice.nsl.sh")
	issue(t, store, acme, "mailhub-203-0-113-10.sslip.io", later, "mailhub-203-0-113-10.sslip.io")
	r, _ := Pick(store, hosts, "", []net.IP{boxIP}, dns("mailhub-alice.nsl.sh"), now)
	if r.Host != "mailhub.alice.nsl.sh" {
		t.Fatalf("got %q", r.Host)
	}
}

func TestHostFrontedByACDNIsSkipped(t *testing.T) {
	// A certificate obtained through the gateway (HTTP-01 forwarded to the box)
	// does not make the host reachable on a port the gateway does not carry.
	store := t.TempDir()
	issue(t, store, acme, "mailhub.alice.nsl.sh", later, "mailhub.alice.nsl.sh")
	r, _ := Pick(store, hosts, "", []net.IP{boxIP}, dns("mailhub.alice.nsl.sh"), now)
	if r.Trusted() {
		t.Fatalf("picked %q, which resolves to the CDN", r.Host)
	}
}

func TestLocalIssuerIsNotTrusted(t *testing.T) {
	store := t.TempDir()
	issue(t, store, LocalIssuer, "wildcard_.alice.nsl.sh", later, "*.alice.nsl.sh")
	r, _ := Pick(store, hosts, "", []net.IP{boxIP}, dns(), now)
	if r.Trusted() {
		t.Fatalf("picked %q from the local CA", r.Host)
	}
}

func TestExpiringOrWrongCertificateIsSkipped(t *testing.T) {
	store := t.TempDir()
	issue(t, store, acme, "mailhub.alice.nsl.sh", now.Add(time.Hour), "mailhub.alice.nsl.sh") // about to expire
	issue(t, store, acme, "mailhub-203-0-113-10.sslip.io", later, "something-else.sslip.io")  // wrong name
	r, _ := Pick(store, hosts, "", []net.IP{boxIP}, dns("mailhub-alice.nsl.sh"), now)
	if r.Trusted() {
		t.Fatalf("picked %q", r.Host)
	}
}

func TestStickyKeepsTheHostInUse(t *testing.T) {
	store := t.TempDir()
	issue(t, store, acme, "wildcard_.alice.nsl.sh", later, "*.alice.nsl.sh")
	issue(t, store, acme, "mailhub-203-0-113-10.sslip.io", later, "mailhub-203-0-113-10.sslip.io")
	// The zone was delegated after the app was set up on sslip.io: stay there.
	r, _ := Pick(store, hosts, "mailhub-203-0-113-10.sslip.io", []net.IP{boxIP}, dns("mailhub-alice.nsl.sh"), now)
	if r.Host != "mailhub-203-0-113-10.sslip.io" {
		t.Fatalf("moved to %q", r.Host)
	}
}

func TestStickyGivesWayWhenTheHostStopsQualifying(t *testing.T) {
	store := t.TempDir()
	issue(t, store, acme, "wildcard_.alice.nsl.sh", later, "*.alice.nsl.sh")
	// The box changed IP: the old sslip.io name is no longer one of its hosts.
	r, _ := Pick(store, hosts, "mailhub-192-0-2-1.sslip.io", []net.IP{boxIP}, dns("mailhub-alice.nsl.sh"), now)
	if r.Host != "mailhub.alice.nsl.sh" {
		t.Fatalf("got %q", r.Host)
	}
}

func TestUnknownBoxAddressSkipsTheDNSCheck(t *testing.T) {
	store := t.TempDir()
	issue(t, store, acme, "mailhub.home.example", later, "mailhub.home.example")
	r, _ := Pick(store, []string{"mailhub.home.example"}, "", nil, dns("mailhub.home.example"), now)
	if r.Host != "mailhub.home.example" {
		t.Fatalf("got %q", r.Host)
	}
}

func TestNoStoreGivesAUsableSelfSignedCertificate(t *testing.T) {
	r, err := Pick("", hosts, "", nil, nil, now)
	if err != nil || r.Trusted() {
		t.Fatalf("got %q, %v", r.Host, err)
	}
	leaf, err := leafOf(r.Cert)
	if err != nil || !keyMatches(leaf, r.Key) || leaf.VerifyHostname(hosts[0]) != nil {
		t.Fatalf("self-signed certificate is not usable: %v", err)
	}
}

func TestWriteKeepsTheSelfSignedCertificateAndReplacesItWithATrustedOne(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	uid, gid := os.Getuid(), os.Getgid()

	first, _ := Pick("", hosts, "", nil, nil, now)
	if changed, err := Write(dir, first, uid, gid); err != nil || !changed {
		t.Fatalf("first write: %v %v", changed, err)
	}
	again, _ := Pick("", hosts, "", nil, nil, now) // a fresh self-signed pair
	if changed, _ := Write(dir, again, uid, gid); changed {
		t.Fatal("a self-signed certificate was replaced by another on a plain re-run")
	}

	store := t.TempDir()
	issue(t, store, acme, "mailhub-203-0-113-10.sslip.io", later, "mailhub-203-0-113-10.sslip.io")
	trusted, _ := Pick(store, hosts, "", []net.IP{boxIP}, dns("mailhub-alice.nsl.sh", "mailhub.alice.nsl.sh"), now)
	if changed, _ := Write(dir, trusted, uid, gid); !changed {
		t.Fatal("the trusted certificate did not replace the self-signed one")
	}
	if changed, _ := Write(dir, trusted, uid, gid); changed {
		t.Fatal("rewriting the same certificate reported a change")
	}
	if st, _ := os.Stat(filepath.Join(dir, KeyFile)); st.Mode().Perm() != 0o600 {
		t.Fatalf("key.pem mode %v", st.Mode().Perm())
	}
}
