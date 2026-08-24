package stackup

import (
	"fmt"
	"os"
	"strings"
)

// This file is the hook execution contract: what an app's lifecycle hook is
// allowed to call, and what it is told when it calls something else.
//
// Hooks run in Maison's own container. That has always been true — it was true
// of the CasaOS fork Maison replaced, whose hooks ran the same `/bin/bash -c`
// in the same kind of container — but two things about it were invisible to app
// authors, and both produced silent failures rather than loud ones:
//
//   - Commands the runtime does not carry (openssl, curl) resolve to nothing.
//     Inside a command substitution that is not an error: `"$(openssl rand -hex
//     32)"` yields the empty string and the surrounding command still exits 0,
//     so an app installs green having written an empty secret.
//   - Commands the runtime *does* carry but cannot honour (sysctl, ip, mount,
//     adduser — busybox ships them all) act on this container instead of the
//     host. They succeed, or fail in a way the hook suppresses, and the setup
//     the author asked for simply never happened.
//
// The answer to both is the same: name the commands hooks may use, and make
// everything else a loud, self-explaining failure. hookBinDir is that list,
// PATH points at it, and command_not_found_handle turns a miss into a message
// that names the sanctioned alternative.

// hookBinDir holds one symlink per command available to hooks. It is built in
// the Dockerfile and it IS the hook ABI — the contract app authors write
// against, in three app stores plus any number of third-party ones. Adding to
// it is a compatible change; removing from it breaks published apps.
//
// Pointing PATH here rather than maintaining a list of *forbidden* commands is
// deliberate: the deny direction is a chase. The container carries 31 host-scoped
// busybox applets today, and an alpine or busybox bump silently adds more. An
// allowlist fails closed — a new applet is unavailable to hooks the moment it
// appears, without anyone noticing it appeared.
//
// A var, not a const, only so tests can point it at a fixture directory.
var hookBinDir = "/opt/maison/hookbin"

// hookFallbackPath is used when hookBinDir is absent — the binary running
// outside its own image, which in practice means local development and `go
// test`. The guardrail switches off rather than breaking every hook on a
// developer's machine: it is an authoring aid, not a security boundary (a hook
// holds the Docker socket, so nothing here contains it), and a guardrail that
// makes the dev loop unusable gets deleted rather than obeyed.
const hookFallbackPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// hookRejectedVar names the file the preamble appends a rejected command to.
// A file rather than an exit status because the failures worth catching are
// exactly the ones that do not produce an exit status: command_not_found_handle
// fires inside `$(...)` and inside subshells, where its 127 is discarded by the
// surrounding command. An append survives both, so RunHook can fail a hook that
// exited 0.
const hookRejectedVar = "MAISON_HOOK_REJECTED"

// hookDocRef is where the message sends the author. Kept short: it is printed
// into container logs and into the install error the UI shows.
const hookDocRef = "docs/x-compose-app.md (Hooks)"

// hookHints maps a rejected command to the sanctioned way of doing the same
// thing. The message is the documentation — an author who reaches for `sysctl`
// is told the recipe at the one moment they are certain to read it — so these
// are worth keeping accurate as the recipes are verified.
//
// Patterns are bash `case` globs, matched in order.
var hookHints = []struct{ pattern, hint string }{
	{"sysctl", "Host kernel parameters: docker run --rm --privileged --network=host <image> sysctl -w <key>=<value>"},
	{"ip|ifconfig|route|arp", "Host network state: docker run --rm --privileged --network=host <image> <command> ... (this container has its own network namespace)"},
	{"mount|umount|swapon|swapoff|losetup|fdisk|blkid|mknod", "Host filesystems: docker run --rm --privileged -v /:/host <image> chroot /host <command> ..."},
	{"useradd|usermod|userdel|groupadd|adduser|addgroup|deluser|delgroup|passwd|su", "Host accounts: docker run --rm -v /:/host <image> chroot /host <command> ... (this container's user database is discarded on restart)"},
	{"modprobe|insmod|rmmod|depmod|dmesg", "Host kernel: docker run --rm --privileged <image> <command> ..."},
	{"chroot|nsenter|unshare", "Already containerised — this does nothing useful here. To act on the host: docker run --rm -v /:/host <image> chroot /host <command> ..."},
	{"systemctl|service|snap|initctl", "Host services cannot be managed from a hook. See " + hookDocRef + " before relying on this."},
	{"reboot|poweroff|halt|shutdown", "Refused. A hook must never restart the machine."},
	{"openssl", "docker run --rm <image> openssl rand -hex 32 — or read /dev/urandom with od, which is available."},
	{"curl|wget2|python|python3|perl|jq|git|unzip|tar|gzip", "Run it in a pinned container instead: docker run --rm <image> <command> ..."},
}

// hookPreamble is bash sourced before every hook via BASH_ENV. BASH_ENV rather
// than string-concatenation so the hook's own line numbers survive into bash's
// error messages — an author debugging a syntax error should see the line they
// wrote, not that line plus however long this preamble happens to be.
func hookPreamble() string {
	var b strings.Builder
	b.WriteString(`# Generated by Maison (internal/stackup/hookshell.go). Sourced before every
# app lifecycle hook. Do not edit — it is rewritten on every hook run.
__maison_reject() {
  {
    echo "maison: '$1' is not available to app hooks."
    if [ -n "${2:-}" ]; then echo "  $2"; fi
    echo "  Hooks run inside the Maison container, which carries a fixed set of commands."
    echo "  See ` + hookDocRef + ` for the full list and the host-access recipes."
  } >&2
  printf '%s\n' "$1" >> "${` + hookRejectedVar + `:-/dev/null}"
  return 127
}

# Invoked by bash for any command not found on PATH. PATH points at Maison's
# curated command set, so this fires both for commands the container does not
# have and for commands it has but deliberately withholds.
command_not_found_handle() {
  case "$1" in
`)
	for _, h := range hookHints {
		fmt.Fprintf(&b, "    %s) __maison_reject \"$1\" %s ;;\n", h.pattern, bashQuote(h.hint))
	}
	b.WriteString(`    *) __maison_reject "$1" "" ;;
  esac
}
`)
	return b.String()
}

// bashQuote renders s as a bash single-quoted literal, in which every byte but
// the quote itself is literal.
func bashQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hookPath is the PATH a hook runs with: the curated set when it exists, the
// ordinary one when it does not. See hookFallbackPath.
func hookPath() string {
	if fi, err := os.Stat(hookBinDir); err == nil && fi.IsDir() {
		return hookBinDir
	}
	return hookFallbackPath
}

// rejectedCommands reads the names the preamble appended, de-duplicated and in
// first-seen order. A missing or empty file means the hook stayed inside the
// contract.
func rejectedCommands(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}
