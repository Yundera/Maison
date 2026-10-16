package apps

import (
	"crypto/x509"
	"errors"
	"testing"
)

func TestClassifyStatus(t *testing.T) {
	cases := map[int]string{
		200: probeOK,
		204: probeOK,
		301: probeOK, // SSO / index redirect — the app is up
		302: probeOK,
		401: probeOK, // auth challenge — the app is up
		403: probeOK,
		404: probeOK, // wrong path, but the server answered
		499: probeOK,
		500: probeAppError,
		501: probeAppError,
		502: probeBadGateway,
		503: probeBadGateway,
		504: probeBadGateway,
		505: probeAppError,
	}
	for code, want := range cases {
		if got := classifyStatus(code); got != want {
			t.Errorf("classifyStatus(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestClassifyErr(t *testing.T) {
	tlsErrs := []error{
		x509.UnknownAuthorityError{},
		x509.CertificateInvalidError{},
		errors.New("x509: certificate signed by unknown authority"),
		errors.New("tls: failed to verify certificate"),
	}
	for _, e := range tlsErrs {
		if got := classifyErr(e); got != probeTLS {
			t.Errorf("classifyErr(%v) = %q, want %q", e, got, probeTLS)
		}
	}
	netErrs := []error{
		errors.New("dial tcp 10.0.0.1:443: connect: connection refused"),
		errors.New("context deadline exceeded"),
		errors.New("no such host"),
	}
	for _, e := range netErrs {
		if got := classifyErr(e); got != probeNet {
			t.Errorf("classifyErr(%v) = %q, want %q", e, got, probeNet)
		}
	}
}

func TestMarkerName(t *testing.T) {
	cases := map[string]string{
		"nextcloud":     "nextcloud",
		"my-app_1":      "my-app_1",
		"../../etc/pwd": "______etc_pwd",
		"a/b":           "a_b",
	}
	for in, want := range cases {
		if got := markerName(in); got != want {
			t.Errorf("markerName(%q) = %q, want %q", in, got, want)
		}
	}
}
