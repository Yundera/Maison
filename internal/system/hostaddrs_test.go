package system

import (
	"strings"
	"testing"
)

// A host with eth0 on a public address, the Docker bridge, and WireGuard Easy - Host's
// wg0. Trimmed from a real /proc/net/fib_trie; the Local table repeats the leaves.
const fibTrie = `Main:
  +-- 0.0.0.0/0 3 0 5
     |-- 0.0.0.0
        /0 universe UNICAST
     +-- 10.9.0.0/24 2 0 2
        |-- 10.9.0.0
           /24 link UNICAST
     |-- 161.97.177.0
        /24 link UNICAST
Local:
  +-- 0.0.0.0/0 3 0 5
     +-- 10.9.0.0/24 2 0 2
        |-- 10.9.0.1
           /32 host LOCAL
        |-- 10.9.0.255
           /32 link BROADCAST
     |-- 127.0.0.0
        /8 host LOCAL
     |-- 161.97.177.252
        /32 host LOCAL
     |-- 172.17.0.1
        /32 host LOCAL
`

const ifInet6 = `00000000000000000000000000000001 01 80 10 80       lo
fe80000000000000c04e62fffeb53e2c 09 40 20 80  docker0
`

func TestParseFibTrieKeepsOnlyLocalLeaves(t *testing.T) {
	got := parseFibTrie(strings.NewReader(fibTrie))
	for _, want := range []string{"10.9.0.1", "161.97.177.252", "172.17.0.1"} {
		if !got[want] {
			t.Errorf("%s missing from %v", want, got)
		}
	}
	// A network, a broadcast and a /8 route are not addresses the host owns one by one.
	for _, not := range []string{"10.9.0.0", "10.9.0.255", "161.97.177.0", "127.0.0.0"} {
		if got[not] {
			t.Errorf("%s should not be local", not)
		}
	}
}

func TestParseIfInet6(t *testing.T) {
	got := parseIfInet6(strings.NewReader(ifInet6))
	if !got["::1"] || !got["fe80::c04e:62ff:feb5:3e2c"] {
		t.Errorf("got %v", got)
	}
}

func TestIsHostAddress(t *testing.T) {
	local := parseFibTrie(strings.NewReader(fibTrie))
	cases := map[string]bool{
		"10.9.0.1":  true,
		"10.9.0.11": false, // the typo this exists to catch
		"127.0.0.5": true,  // loopback is one route, never a leaf per address
		"::1":       true,
		"not-an-ip": false,
	}
	for addr, want := range cases {
		if got := IsHostAddress(local, addr); got != want {
			t.Errorf("IsHostAddress(%q) = %v, want %v", addr, got, want)
		}
	}
}
