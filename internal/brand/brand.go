// Package brand holds every string that carries the product's identity, so a
// rebrand is a change to this file and nothing else.
//
// The two constants are not interchangeable, and the distinction is the whole
// point of the package:
//
//   - Name is a DISPLAY string. It appears in the UI, in log prefixes, and in the
//     comments Maison writes into an operator's compose override. Nothing ever
//     reads it back, so changing it is free — it can gain a space, an accent, a
//     second word.
//
//   - Slug is an IDENTIFIER. It names the state directory on disk, Maison's own
//     gateway host, its control endpoints and its response header. Every one of
//     those is a contract with something outside this binary — a deployment's
//     compose file, a user's data folder, the reverse proxy — so changing it is a
//     breaking change that has to land together with template-root and the app
//     store. It must stay a lowercase DNS label: it becomes a hostname.
//
// Everything else Maison writes into a file it does not own is deliberately
// brand-free: the compose bookkeeping key is `generated-routes` inside the
// standard `x-compose-app` block, and the environment variables are HTTP_ADDR /
// STATE_DIR. Those are not derived from anything here, and must not be — the
// point is that a future rename cannot reach them.
package brand

const (
	// Name is the product's display name.
	Name = "Maison"

	// Slug is the identifier form of the name: lowercase, DNS-safe, no spaces.
	Slug = "maison"

	// Header marks every response Maison serves, so a probe that lands on the
	// catch-all can tell "the app answered" from "the dashboard answered for it"
	// (see internal/apps.Reach). Go canonicalises it to X-Maison on the wire.
	Header = "X-" + Slug

	// GateRoot prefixes the control endpoints the launch page calls while an app
	// is still down (see internal/server/gate.go). Double-underscore so it cannot
	// collide with a real path on the app host it is standing in for.
	GateRoot = "/__" + Slug
)
