package installer

import (
	"errors"
	"testing"

	"github.com/yundera/maison/internal/xcomposeapp"
)

func TestCheckInputs(t *testing.T) {
	declared := []xcomposeapp.Input{{Env: "TS_AUTHKEY"}, {Env: "PLEX_CLAIM"}}
	if err := checkInputs(declared, map[string]string{"TS_AUTHKEY": "tskey-1", "PLEX_CLAIM": ""}); err != nil {
		t.Fatalf("declared keys refused: %v", err)
	}
	if err := checkInputs(nil, nil); err != nil {
		t.Fatalf("no inputs refused: %v", err)
	}
	// The store is not a way to set an arbitrary variable.
	if err := checkInputs(declared, map[string]string{"PUID": "0"}); !errors.Is(err, ErrBadInput) {
		t.Fatalf("undeclared key accepted: %v", err)
	}
	if err := checkInputs(declared, map[string]string{"TS_AUTHKEY": "a\nPUID=0"}); !errors.Is(err, ErrBadInput) {
		t.Fatalf("multi-line value accepted: %v", err)
	}
}

func TestFilledInputsSkipsBlanks(t *testing.T) {
	got := filledInputs(map[string]string{"A": " x ", "B": "  ", "C": ""})
	if len(got) != 1 || got["A"] != "x" {
		t.Fatalf("filledInputs = %v", got)
	}
}
