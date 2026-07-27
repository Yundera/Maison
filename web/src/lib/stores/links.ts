import { writable } from 'svelte/store'

// External-link tiles are a purely client-side convenience for now; they move
// server-side with the rest of settings in M5.
export interface Link {
  id: string
  name: string
  url: string
  icon?: string
}

function load(): Link[] {
  try {
    const raw = localStorage.getItem('maison.links')
    if (raw) return JSON.parse(raw)
  } catch {
    /* ignore */
  }
  return []
}

export const links = writable<Link[]>(load())

links.subscribe((v) => {
  try {
    localStorage.setItem('maison.links', JSON.stringify(v))
  } catch {
    /* ignore */
  }
})

export function addLink(name: string, url: string, icon?: string): void {
  const id = 'link:' + Math.random().toString(36).slice(2, 9)
  links.update((v) => [...v, { id, name, url, icon }])
}

export function removeLink(id: string): void {
  links.update((v) => v.filter((l) => l.id !== id))
}
