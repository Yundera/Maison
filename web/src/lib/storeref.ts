// A store reference addresses one app in one store:
//
//   <locator>/-/<in-zip path>
//   git.example.org/appstore/archive/main.zip/-/Apps/FileBrowser
//
// The locator is a URL whose scheme may be omitted (https is implied); the in-zip
// path is the app's directory relative to the store root, so the apps folder is
// named by the reference rather than hardcoded. Both halves are needed: a box
// asked for a store it has never seen must be told *where* it lives, which no
// identifier can carry, while the folder inside the archive is the store's
// layout and not the box's business.
//
// This is the mirror of internal/appstore/ref.go, which is the authority. The two
// exist because the SPA owns the URL and the server owns the fetch; keep them in
// step, and prefer changing ref.go first.

/** The separator, borrowed from GitLab. */
const SEP = '/-/'

/** The apps folder assumed when a reference names none — the CasaOS layout every
 *  store uses today, so old links keep resolving unchanged. */
export const DEFAULT_APPS_PATH = 'Apps'

export interface StoreRef {
  /** Store locator; '' means the merged catalog answers. */
  url: string
  /** Apps folder inside the archive; '' means DEFAULT_APPS_PATH. */
  appsPath: string
  /** The app's directory name — the catalog id. '' means the catalog itself. */
  id: string
}

export const CATALOG: StoreRef = { url: '', appsPath: '', id: '' }

export const appsOf = (r: StoreRef): string => r.appsPath || DEFAULT_APPS_PATH

/** Parse the part of a deep link after /store/.
 *
 *  Split on the LAST separator, never the first: a GitLab-hosted store's own
 *  archive URL contains one —
 *  `git.example.org/group/project/-/archive/main/project-main.zip` — and
 *  splitting on the first would fetch `git.example.org/group/project` and look
 *  for the app under `archive/...`. A different store, with no error to say so.
 *
 *  With no separator at all the whole input is an in-zip path against the merged
 *  catalog, so `/store/FileBrowser` keeps meaning what it always meant. */
export function parseRef(s: string): StoreRef {
  const trimmed = s.replace(/^\/+|\/+$/g, '')
  if (!trimmed) return CATALOG
  const i = trimmed.lastIndexOf(SEP)
  if (i < 0) return { ...splitInZip(trimmed), url: '' }
  return { ...splitInZip(trimmed.slice(i + SEP.length)), url: trimmed.slice(0, i) }
}

/** Render a reference as it appears in a deep link: scheme-less, because https is
 *  implied and those eight characters are the ugliest part of an otherwise
 *  readable address. Round-trips through parseRef. */
export function refPath(r: StoreRef): string {
  // Without a locator the apps folder is left off: it would assert something
  // about a store the reference does not name, and would rewrite every plain
  // /store/<id> link into a longer one that says no more than it did.
  if (!r.url) return r.appsPath ? `${r.appsPath}/${r.id}` : r.id
  return `${bare(r.url)}${SEP}${appsOf(r)}/${r.id}`
}

/** The same reference as API query parameters. Encoded in full here, unlike in a
 *  deep link: nobody reads this one, and `&` in a locator would end the value. */
export function refQuery(r: StoreRef): string {
  const q: string[] = []
  if (r.url) q.push(`store=${encodeURIComponent(r.url)}`)
  if (r.appsPath && r.appsPath !== DEFAULT_APPS_PATH) {
    q.push(`apps_path=${encodeURIComponent(r.appsPath)}`)
  }
  return q.length ? `?${q.join('&')}` : ''
}

/** The reference a catalog entry came from, so an install goes back to the store
 *  and the folder the app was read from rather than to the merged catalog. */
export function refOf(app: { id: string; store?: string; apps_path?: string }): StoreRef {
  return { url: app.store ?? '', appsPath: app.apps_path ?? '', id: app.id }
}

/** Drop an implied scheme for display. http is kept: it is not the default, and a
 *  store fetched over it is one anyone on the path can replace. */
const bare = (u: string): string => u.replace(/^https:\/\//, '')

/** The last segment is the app; everything before it is the apps folder. */
function splitInZip(p: string): StoreRef {
  const path = p.replace(/^\/+|\/+$/g, '')
  const i = path.lastIndexOf('/')
  return i < 0
    ? { url: '', appsPath: '', id: path }
    : { url: '', appsPath: path.slice(0, i), id: path.slice(i + 1) }
}
