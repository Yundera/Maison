// Package secretgen generates the values an app declares under x-compose-app
// `secrets`: a random string of a given shape, or a bcrypt hash of a rendered
// template.
//
// It exists because the alternative — a shell hook calling openssl — fails
// silently. openssl is not in Maison's runtime image, so `"$(openssl rand -hex
// 32)"` expands to the empty string while the surrounding command still exits 0,
// and an app installs green having written an empty secret it will never
// regenerate (its own `[ ! -s ]` guard sees a non-empty file). A generator
// compiled into the binary cannot be missing and cannot half-succeed.
//
// The vocabulary deliberately mirrors the openssl invocations the store's apps
// already copied, so a migrated app produces the same shape of value it did
// before: `hex:32` is `openssl rand -hex 32` (32 BYTES, 64 characters) and
// `base64:32` is `openssl rand -base64 32`.
package secretgen

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// maxSize bounds what a store may ask for, in bytes or characters. A generator
// spec comes from a downloaded store, so an unbounded count is a way to make an
// install allocate until it dies. Nothing legitimate needs more than this.
const maxSize = 4096

// bcryptCost is the work factor for `bcrypt:`. 10 is bcrypt's own default and
// what `dex hash` produces, which is the hash this generator replaces.
const bcryptCost = 10

// alphabet is the character set for `alnum:` and `password:`. Letters and digits
// only: a generated value has to survive being written into a .env line, quoted
// into a YAML config, and pasted into a URL, and punctuation loses at least one
// of those. Randomness comes from the length, not from the symbol set.
const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Generate produces the value for one `secrets` spec.
//
//	hex:N        N random bytes, hex-encoded      (2N characters)
//	base64:N     N random bytes, base64-encoded   (standard alphabet, padded)
//	alnum:N      N random characters from [A-Za-z0-9]
//	password:N   alias for alnum:N, for the call site that reads better
//	uuid         a random (v4) UUID
//	bcrypt:TEXT  bcrypt hash of TEXT, which the caller has already rendered
//
// The error names the spec, because it surfaces to whoever is installing the app
// and the app's author is not in the room.
func Generate(spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	kind, arg, hasArg := strings.Cut(spec, ":")
	kind = strings.ToLower(strings.TrimSpace(kind))

	switch kind {
	case "uuid":
		if hasArg {
			return "", fmt.Errorf("%q: uuid takes no argument", spec)
		}
		return uuidV4()

	case "bcrypt":
		// Not size-checked and not trimmed: the argument is a rendered template,
		// so its content is the app's business. Empty is refused because bcrypt
		// accepts it happily and the result is a hash of nothing — which in every
		// case this replaces means a variable that failed to resolve.
		if !hasArg || arg == "" {
			return "", fmt.Errorf("%q: bcrypt needs a value to hash, e.g. bcrypt:${APP_DEFAULT_PASSWORD}", spec)
		}
		h, err := bcrypt.GenerateFromPassword([]byte(arg), bcryptCost)
		if err != nil {
			return "", fmt.Errorf("%q: %w", spec, err)
		}
		return string(h), nil

	case "hex", "base64", "alnum", "password":
		n, err := size(spec, arg, hasArg)
		if err != nil {
			return "", err
		}
		switch kind {
		case "hex":
			b, err := randomBytes(n)
			if err != nil {
				return "", err
			}
			return hex.EncodeToString(b), nil
		case "base64":
			b, err := randomBytes(n)
			if err != nil {
				return "", err
			}
			return base64.StdEncoding.EncodeToString(b), nil
		default:
			return randomChars(n)
		}
	}
	return "", fmt.Errorf("%q: unknown generator %q — use hex:N, base64:N, alnum:N, password:N, uuid or bcrypt:TEXT", spec, kind)
}

// seedRe is one seed alternative: exactly a `${NAME}` reference, nothing around it.
var seedRe = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// ParseSpec splits a raw, unrendered `secrets` spec into the variables it seeds
// from and the generator that runs when none of them has a value:
//
//	${APP_DEFAULT_PASSWORD}|alnum:32   → seeds [APP_DEFAULT_PASSWORD], gen "alnum:32"
//	hex:32                             → no seeds, gen "hex:32"
//
// The form exists to move an installed app off a shared value without changing
// it under the app: the caller resolves each seed against what the app already
// had, and only a fresh install reaches the generator.
//
// It must run on the RAW spec. Splitting after rendering would let a `|` inside a
// password cut it in two — and is why a spec only counts as seeded when every
// part before the last is exactly one `${NAME}`. Anything else is returned whole
// as the generator, so `bcrypt:${X}` and every spec written before this form
// behave exactly as they did.
func ParseSpec(raw string) (seeds []string, gen string) {
	raw = strings.TrimSpace(raw)
	parts := strings.Split(raw, "|")
	if len(parts) < 2 {
		return nil, raw
	}
	for _, p := range parts[:len(parts)-1] {
		m := seedRe.FindStringSubmatch(strings.TrimSpace(p))
		if m == nil {
			return nil, raw
		}
		seeds = append(seeds, m[1])
	}
	return seeds, strings.TrimSpace(parts[len(parts)-1])
}

// size parses and bounds the N of a sized generator.
func size(spec, arg string, hasArg bool) (int, error) {
	if !hasArg {
		return 0, fmt.Errorf("%q: needs a size, e.g. hex:32", spec)
	}
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil {
		return 0, fmt.Errorf("%q: size %q is not a number", spec, arg)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%q: size must be positive", spec)
	}
	if n > maxSize {
		return 0, fmt.Errorf("%q: size must be at most %d", spec, maxSize)
	}
	return n, nil
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// randomChars draws n characters from alphabet without modulo bias: a byte at or
// above the largest whole multiple of len(alphabet) is discarded and redrawn,
// which is what keeps every character equally likely.
func randomChars(n int) (string, error) {
	const limit = 256 - (256 % len(alphabet))
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) < limit {
				out = append(out, alphabet[int(b)%len(alphabet)])
				if len(out) == n {
					break
				}
			}
		}
	}
	return string(out), nil
}

// uuidV4 formats 16 random bytes as a version-4 UUID.
func uuidV4() (string, error) {
	b, err := randomBytes(16)
	if err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
