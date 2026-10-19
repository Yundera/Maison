package system

import (
	"bufio"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// HostAddresses returns every address the HOST carries, IPv4 and IPv6, as strings in
// net.IP's canonical form. ok is false when the host's /proc is not reachable, and the
// caller must then judge nothing: this container's own addresses are not the host's.
//
// It reads PID 1's files for the same reason readNetDev does — /proc/net resolves in the
// reader's network namespace however it is reached, and only an explicit PID gets the
// host's.
//
// IPv4 comes from fib_trie rather than an interface walk because that is the only /proc
// file that lists them: a "/32 host LOCAL" leaf is an address the kernel will accept as
// its own. IPv6 comes from if_inet6, one address per line.
func HostAddresses() (addrs map[string]bool, ok bool) {
	if !HasHostProc() {
		return nil, false
	}
	f, err := os.Open(filepath.Join(hostProc(), "1", "net", "fib_trie"))
	if err != nil {
		return nil, false
	}
	addrs = parseFibTrie(f)
	f.Close()
	// IPv6 may be disabled on the host; an absent if_inet6 is no IPv6, not no answer.
	if f6, err := os.Open(filepath.Join(hostProc(), "1", "net", "if_inet6")); err == nil {
		for a := range parseIfInet6(f6) {
			addrs[a] = true
		}
		f6.Close()
	}
	return addrs, true
}

// IsHostAddress reports whether addr is one the host can bind and receive on. Loopback
// is always local, whatever the table says: 127.0.0.0/8 is one route, not a leaf per
// address.
func IsHostAddress(addrs map[string]bool, addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	return addrs[ip.String()]
}

// parseFibTrie collects the local addresses from /proc/net/fib_trie:
//
//	|-- 10.9.0.1
//	   /32 host LOCAL
//
// The leaf line names the address, the line under it says what kind of route it is.
// Both the Main and Local tables are printed; the map absorbs the repeat.
func parseFibTrie(r io.Reader) map[string]bool {
	out := map[string]bool{}
	last := ""
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if a, ok := strings.CutPrefix(line, "|-- "); ok {
			last = a
			continue
		}
		if last != "" && strings.HasPrefix(line, "/32 host LOCAL") {
			if ip := net.ParseIP(last); ip != nil {
				out[ip.String()] = true
			}
		}
	}
	return out
}

// parseIfInet6 reads /proc/net/if_inet6: the address as 32 hex digits, then the
// interface index, prefix length, scope, flags and name.
func parseIfInet6(r io.Reader) map[string]bool {
	out := map[string]bool{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || len(fields[0]) != 32 {
			continue
		}
		b, err := hex.DecodeString(fields[0])
		if err != nil {
			continue
		}
		out[net.IP(b).String()] = true
	}
	return out
}
