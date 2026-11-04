// Package certs serves an app's `certificates`: a TLS certificate for the hosts
// of one of its routes, for a listener the proxy does not carry (IMAPS, MQTT over
// TLS). The proxy already obtains a certificate for every routed host; this
// package finds the best of them in the proxy's store and hands the app a copy,
// so the app never learns where the store is or which domain has a real
// certificate on this box.
//
// "Best" is the first host, in the routing setting's domain order (primary
// first), that
//
//   - has a certificate in the store from a public issuer (an ACME CA — never the
//     proxy's own local CA), unexpired and valid for the host, either its own or
//     a wildcard one level up;
//   - and resolves straight to the box (one of its public IPs), since a client
//     connecting to a port the proxy does not carry must reach the box itself —
//     a host behind a gateway or CDN is reachable over HTTPS only.
//
// The choice is *sticky*: the host already in use (prefer) is kept as long as it
// still qualifies, even when a host earlier in the order has become available
// (its zone delegated, a domain added). Clients are configured with that name —
// a phone's mail account — and moving the certificate to another name would
// break every one of them for no gain. It is checked again on every up and on
// every periodic check, so a renewal reaches the app, and a host that stops
// qualifying (an IP change, a domain removed) is replaced by the next best one.
//
// When no host qualifies, Pick returns a self-signed certificate, so the app
// still starts, and an empty Host, so it can tell its users there is no trusted
// name yet.
package certs

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalIssuer is the directory Caddy keeps its own CA's certificates under —
// `tls internal`, trusted by nothing outside the box.
const LocalIssuer = "local"

// MinValidity is how long a certificate must still be valid to be picked: one
// that expires sooner is about to be renewed, and the renewal is picked instead.
const MinValidity = 24 * time.Hour

// Result is the certificate picked for a set of hosts.
type Result struct {
	// Host is the host the certificate was picked for, the name clients must
	// connect to. Empty for a self-signed certificate.
	Host string
	// Cert is the PEM chain (leaf first), Key the PEM private key.
	Cert, Key []byte
}

// Trusted reports whether r came from the store rather than being self-signed.
func (r Result) Trusted() bool { return r.Host != "" }

// Lookup resolves a host to its addresses. net.LookupIP in production; a test
// passes its own.
type Lookup func(host string) ([]net.IP, error)

// Pick returns the certificate for prefer, when it is one of hosts and still
// qualifies, or else for the first of hosts that has a trusted one in store and
// resolves to one of boxIPs. An empty boxIPs skips the address check
// (the box's public address is not known, e.g. on a LAN). An empty store, or no
// qualifying host, gives a self-signed certificate.
func Pick(store string, hosts []string, prefer string, boxIPs []net.IP, lookup Lookup, now time.Time) (Result, error) {
	if store != "" {
		for _, h := range withFirst(hosts, prefer) {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" || net.ParseIP(h) != nil {
				continue
			}
			if !reachesBox(h, boxIPs, lookup) {
				continue
			}
			if cert, key, ok := find(store, h, now); ok {
				return Result{Host: h, Cert: cert, Key: key}, nil
			}
		}
	}
	name := "localhost"
	if len(hosts) > 0 && hosts[0] != "" {
		name = hosts[0]
	}
	cert, key, err := SelfSigned(name, now)
	return Result{Cert: cert, Key: key}, err
}

// reachesBox reports whether host resolves to one of the box's addresses.
func reachesBox(host string, boxIPs []net.IP, lookup Lookup) bool {
	if len(boxIPs) == 0 {
		return true
	}
	if lookup == nil {
		lookup = net.LookupIP
	}
	ips, err := lookup(host)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		for _, b := range boxIPs {
			if ip.Equal(b) {
				return true
			}
		}
	}
	return false
}

// find looks for host's certificate in a Caddy-layout store: every issuer
// directory but the local CA's, the host's own entry first, then the wildcard
// one level up.
func find(store, host string, now time.Time) (cert, key []byte, ok bool) {
	issuers, err := os.ReadDir(store)
	if err != nil {
		return nil, nil, false
	}
	names := []string{host}
	if i := strings.IndexByte(host, '.'); i > 0 && strings.Contains(host[i+1:], ".") {
		names = append(names, "wildcard_."+host[i+1:])
	}
	for _, name := range names {
		for _, iss := range issuers {
			if !iss.IsDir() || iss.Name() == LocalIssuer {
				continue
			}
			dir := filepath.Join(store, iss.Name(), name)
			c, err1 := os.ReadFile(filepath.Join(dir, name+".crt"))
			k, err2 := os.ReadFile(filepath.Join(dir, name+".key"))
			if err1 != nil || err2 != nil {
				continue
			}
			if usable(c, k, host, now) {
				return c, k, true
			}
		}
	}
	return nil, nil, false
}

// usable checks the leaf certificate covers host, is valid for at least
// MinValidity, and matches the key.
func usable(certPEM, keyPEM []byte, host string, now time.Time) bool {
	leaf, err := leafOf(certPEM)
	if err != nil {
		return false
	}
	if now.Before(leaf.NotBefore) || now.Add(MinValidity).After(leaf.NotAfter) {
		return false
	}
	if leaf.VerifyHostname(host) != nil {
		return false
	}
	return keyMatches(leaf, keyPEM)
}

func leafOf(certPEM []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(certPEM)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("no certificate")
	}
	return x509.ParseCertificate(b.Bytes)
}

func keyMatches(leaf *x509.Certificate, keyPEM []byte) bool {
	b, _ := pem.Decode(keyPEM)
	if b == nil {
		return false
	}
	var pub crypto.PublicKey
	if k, err := x509.ParsePKCS8PrivateKey(b.Bytes); err == nil {
		if s, ok := k.(crypto.Signer); ok {
			pub = s.Public()
		}
	} else if k, err := x509.ParseECPrivateKey(b.Bytes); err == nil {
		pub = k.Public()
	} else if k, err := x509.ParsePKCS1PrivateKey(b.Bytes); err == nil {
		pub = k.Public()
	}
	eq, ok := pub.(interface{ Equal(crypto.PublicKey) bool })
	return ok && eq.Equal(leaf.PublicKey)
}

// SelfSigned makes a certificate for name, valid for a year: what an app gets
// when the box has no trusted certificate for it, so it still starts.
func SelfSigned(name string, now time.Time) (cert, key []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name, Organization: []string{"Maison (self-signed)"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}
	kder, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), nil
}

// Files are the two files a certificate is written as.
const (
	CertFile = "cert.pem"
	KeyFile  = "key.pem"
)

// Write puts r into dir as cert.pem and key.pem, owned by uid:gid, and reports
// whether either file changed. A self-signed certificate is kept rather than
// replaced by a fresh one on every up: it only changes when a trusted one
// arrives.
func Write(dir string, r Result, uid, gid int) (bool, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	certPath, keyPath := filepath.Join(dir, CertFile), filepath.Join(dir, KeyFile)
	oldCert, _ := os.ReadFile(certPath)
	oldKey, _ := os.ReadFile(keyPath)
	if !r.Trusted() && isSelfSigned(oldCert) && len(oldKey) > 0 {
		r.Cert, r.Key = oldCert, oldKey
	}
	changed := !bytes.Equal(oldCert, r.Cert) || !bytes.Equal(oldKey, r.Key)
	if changed {
		// Key first: a reader that sees the new chain must find its key.
		if err := writeFile(keyPath, r.Key, 0o600); err != nil {
			return false, err
		}
		if err := writeFile(certPath, r.Cert, 0o644); err != nil {
			return false, err
		}
	}
	for _, p := range []string{keyPath, certPath} {
		if err := os.Chown(p, uid, gid); err != nil && !errors.Is(err, os.ErrPermission) {
			return changed, fmt.Errorf("chown %s: %w", p, err)
		}
	}
	return changed, nil
}

func isSelfSigned(certPEM []byte) bool {
	leaf, err := leafOf(certPEM)
	return err == nil && bytes.Equal(leaf.RawIssuer, leaf.RawSubject)
}

// writeFile replaces path atomically with data at mode.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// withFirst is hosts with prefer moved to the front, when it is one of them.
func withFirst(hosts []string, prefer string) []string {
	prefer = strings.ToLower(strings.TrimSpace(prefer))
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if strings.EqualFold(h, prefer) {
			out = append([]string{h}, out...)
		} else {
			out = append(out, h)
		}
	}
	return out
}
