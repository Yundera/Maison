import { writable, derived } from 'svelte/store'
import { BRAND } from '../brand'
import en_us from './lang/en_us.json'
import fr_fr from './lang/fr_fr.json'
import de_de from './lang/de_de.json'
import zh_cn from './lang/zh_cn.json'

type Dict = Record<string, string>

// One JSON per language, keyed by semantic id (CasaOS ships 31 languages; the
// structure here supports adding the rest without code changes).
const messages: Record<string, Dict> = { en_us, fr_fr, de_de, zh_cn }

export const languages = [
  { code: 'en_us', name: 'English' },
  { code: 'fr_fr', name: 'Français' },
  { code: 'de_de', name: 'Deutsch' },
  { code: 'zh_cn', name: '中文' },
]

export const locale = writable('en_us')

// Placeholders every message may use without the call site passing anything.
// `{app}` is the product name: translators write the placeholder rather than the
// name, so a rebrand does not invalidate a single translated string. See lib/brand.
const globals: Record<string, string> = { app: BRAND }

const PLACEHOLDER = /\{(\w+)\}/g

/**
 * Reactive translator: `$t('app')`, or `$t('key', { count: '3' })`.
 *
 * `{name}` placeholders are substituted from `params` first, then from `globals`;
 * an unknown one is left as written, so a typo shows up on screen instead of
 * silently emptying the sentence. Falls back to en_us, then to the key itself.
 */
export const t = derived(
  locale,
  ($l) =>
    (key: string, params?: Record<string, string>): string => {
      const raw = messages[$l]?.[key] ?? messages['en_us'][key] ?? key
      return raw.replace(PLACEHOLDER, (m, k) => params?.[k] ?? globals[k] ?? m)
    },
)

/** A user-facing string an app declares in x-compose-app: a locale map that
 *  always carries en_us (docs/x-compose-app.md). */
export type Localized = Record<string, string>

/** The entry of `m` for locale `l`, else its en_us, else `fallback`. */
export function pick(m: Localized | null | undefined, l: string, fallback = ''): string {
  return m?.[l] || m?.en_us || fallback
}

/** Reactive pick in the current locale: `$loc(app.tagline)`, `$loc(app.title, app.name)`. */
export const loc = derived(
  locale,
  ($l) =>
    (m: Localized | null | undefined, fallback = ''): string =>
      pick(m, $l, fallback),
)
