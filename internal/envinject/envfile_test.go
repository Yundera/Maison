package envinject

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/config"
)

func TestParseEnvFile(t *testing.T) {
	in := []byte("# seeded at install\nPUID=1000\n\nTZ = Europe/Paris\nbogus-line\nPUID=1001\n")
	got := ParseEnvFile(in)
	want := []Var{
		{Key: "PUID", Value: "1001"}, // duplicate: first position, last value — as compose resolves it
		{Key: "TZ", Value: "Europe/Paris"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseEnvFile() = %#v, want %#v", got, want)
	}
}

// The whole point of patching rather than regenerating: an operator's comments,
// blank lines and key order survive a save from the editor.
func TestPatchEnvFilePreservesCommentsAndOrder(t *testing.T) {
	old := []byte("# Maison seeded these\nPUID=1000\nPGID=1000\n\n# my own note\nTZ=UTC\n")
	got, err := PatchEnvFile(old, []Var{
		{Key: "PUID", Value: "1000"},
		{Key: "TZ", Value: "Europe/Paris"}, // edited
		{Key: "API_KEY", Value: "s3cret"},  // added
		// PGID removed
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "# Maison seeded these\nPUID=1000\n\n# my own note\nTZ=Europe/Paris\nAPI_KEY=s3cret\n"
	if string(got) != want {
		t.Fatalf("PatchEnvFile() =\n%q\nwant\n%q", got, want)
	}
}

func TestPatchEnvFileEmptiedFile(t *testing.T) {
	got, err := PatchEnvFile([]byte("PUID=1000\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("PatchEnvFile(all removed) = %q, want nil", got)
	}
}

func TestPatchEnvFileRejectsBadVars(t *testing.T) {
	cases := map[string][]Var{
		"empty key":       {{Key: "", Value: "x"}},
		"leading digit":   {{Key: "2FA", Value: "x"}},
		"shell-unsafe":    {{Key: "MY-KEY", Value: "x"}},
		"duplicate":       {{Key: "A", Value: "1"}, {Key: "A", Value: "2"}},
		"multiline value": {{Key: "A", Value: "one\ntwo"}},
	}
	for name, vars := range cases {
		if _, err := PatchEnvFile([]byte("A=0\n"), vars); err == nil {
			t.Errorf("PatchEnvFile(%s) succeeded, want error", name)
		}
	}
}

// A value may legitimately contain '=', spaces or a '#' — .env has no escaping,
// so everything after the first '=' is the value and must round-trip verbatim.
func TestPatchEnvFileValueRoundTrip(t *testing.T) {
	vars := []Var{{Key: "DSN", Value: "user:p@ss=w0rd@host/db?x=1 #notacomment"}}
	got, err := PatchEnvFile(nil, vars)
	if err != nil {
		t.Fatal(err)
	}
	if back := ParseEnvFile(got); !reflect.DeepEqual(back, vars) {
		t.Fatalf("round-trip = %#v, want %#v", back, vars)
	}
}

// ValidateEnvFile is what stands between a raw editor and a file the apps on the
// box depend on, so it must reject anything ParseEnvFile would quietly drop or
// resolve differently — while leaving comments, blanks and empty values alone.
func TestValidateEnvFile(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want string // substring of the expected error; "" = must pass
	}{
		"comments blanks and empty values": {
			raw: "# a note\n\nAPP_DOMAIN=\nAPP_NET=mesh\n",
		},
		"no trailing newline": {
			raw: "APP_NET=mesh",
		},
		"line that is not an entry": {
			// The typo that motivates the whole check: ParseEnvFile skips this
			// line, so without it the save would "succeed" and change nothing.
			raw:  "APP_NET=mesh\nAPP_DOMAIN example.com\n",
			want: "line 2",
		},
		"invalid key": {
			raw:  "1BAD=x\n",
			want: "invalid variable name",
		},
		"duplicate key": {
			// ParseEnvFile takes the last value, so the first would vanish.
			raw:  "APP_NET=mesh\nAPP_NET=pcs\n",
			want: "duplicate",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateEnvFile([]byte(tc.raw))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("ValidateEnvFile() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateEnvFile() = nil, want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ValidateEnvFile() = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// DerivedKeys reads its names off BaseVars so the two cannot drift.
func TestDerivedKeysTracksBaseVars(t *testing.T) {
	got := DerivedKeys()
	base := BaseVars(config.Config{}, "psitransfer")
	if len(got) != len(base) {
		t.Fatalf("DerivedKeys() = %v (%d keys), want the %d BaseVars computes", got, len(got), len(base))
	}
	for _, k := range got {
		if _, ok := base[k]; !ok {
			t.Errorf("DerivedKeys() reports %q, which BaseVars does not compute", k)
		}
	}
	if !slices.IsSorted(got) {
		t.Errorf("DerivedKeys() = %v, want sorted", got)
	}
}
