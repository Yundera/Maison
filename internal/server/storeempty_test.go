package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/yundera/maison/internal/config"
)

// An empty catalog must go out as [], never null.
//
// This is the shape a box has whenever its store has not synced — a fresh
// install, an origin that is unreachable, or a configured branch that has been
// deleted (a 404 leaves the catalog nil and Refresh keeps going). Go marshals a
// nil slice as `null`, so the list fields silently changed type with their
// length, and the store page's `data.apps.filter(...)` threw inside a Svelte
// derived. Because that throw happens mid-render the panel never left its
// loading branch: the store hung on "Loading…" forever instead of showing an
// empty grid. Serving [] is what keeps "no apps" an ordinary render.
//
// The server is built with no store URLs configured, which is that state exactly
// and needs no network.
func TestStoreEndpointsServeEmptyListsNotNull(t *testing.T) {
	h := New(config.Config{DataRoot: t.TempDir()}, fstest.MapFS{})

	for _, path := range []string{"/api/store", "/api/store/sources"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s -> %d, want 200", path, rec.Code)
		}

		// Decoded into any, so this asserts the JSON that reached the wire rather
		// than what a typed struct would helpfully turn null back into.
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("GET %s: %v (body %q)", path, err, rec.Body.String())
		}
		for field, v := range got {
			if v == nil {
				t.Errorf("GET %s: field %q is null, want []", path, field)
				continue
			}
			if _, ok := v.([]any); !ok {
				t.Errorf("GET %s: field %q is %T, want a list", path, field, v)
			}
		}
	}
}
