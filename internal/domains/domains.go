// Package domains describes the additional domains a Maison deployment answers
// on, beyond the primary one the store's compose already routes.
//
// A store app declares exactly one Caddy route, on the deployment's primary
// domain (`jellyfin-${APP_DOMAIN}`). Reaching the same app at a second name —
// an sslip.io / nip.io host, so a box with no DNS is still usable — is a property
// of the *deployment*, not of the app, so it is configured here and Maison
// generates the extra routes (see internal/caddyroutes).
package domains

// Domain is one additional domain every app is published on.
type Domain struct {
	// Name identifies the entry in the settings UI ("sslip", "nip", "lan").
	Name string `json:"name"`

	// Domain is the host suffix, and stays *templated*: it is copied verbatim into
	// the generated Caddy label and resolved by Compose interpolation, exactly like
	// the primary ${APP_DOMAIN} is. Keeping it a template (rather than baking the
	// resolved string in) is what makes the label survive the box changing IP —
	// `${APP_PUBLIC_IP_DASH}.sslip.io` re-resolves on every up.
	Domain string `json:"domain"`

	// Directives are the Caddy sub-directives this domain owns. TLS belongs to the
	// domain, not to the app: the gateway and nip.io hosts carry
	// `import: gateway_tls` (the deployment's custom CA), while sslip.io
	// deliberately carries nothing and falls through to Let's Encrypt. They are
	// applied to every generated route group, replacing whatever the app's own
	// route said about TLS.
	Directives map[string]string `json:"directives,omitempty"`
}
