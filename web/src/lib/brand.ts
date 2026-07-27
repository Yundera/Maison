/**
 * The UI half of the product's identity — the mirror of Go's `internal/brand`.
 *
 * Only the DISPLAY name lives here. Every identifier the browser persists or
 * sends (the `maison.*` localStorage keys, the gate's control paths) stays a
 * literal at its use site on purpose: they are contracts with a stored value or
 * with the server, not with the reader, so a rebrand must not be able to move
 * them by editing one constant.
 *
 * In translated copy the name is written `{app}` and substituted at render time —
 * see `lib/i18n`. Use `BRAND` directly only in untranslated markup.
 */
export const BRAND = 'Maison'
