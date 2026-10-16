/** Svelte action: call `handler` when a click lands outside `node`. */
export function clickOutside(node: HTMLElement, handler: () => void) {
  function onClick(e: MouseEvent) {
    if (!node.contains(e.target as Node)) handler()
  }
  document.addEventListener('click', onClick, true)
  return {
    destroy() {
      document.removeEventListener('click', onClick, true)
    },
  }
}

/** Svelte action: track the pointer over `node` as `--mx`/`--my` (px from its
 * top-left) so CSS can paint a specular highlight that follows it. Mouse only:
 * a touch has no hover to follow, and the highlight would stick where the
 * finger lifted. */
export function pointerGlow(node: HTMLElement) {
  function onMove(e: PointerEvent) {
    if (e.pointerType !== 'mouse') return
    const r = node.getBoundingClientRect()
    node.style.setProperty('--mx', `${e.clientX - r.left}px`)
    node.style.setProperty('--my', `${e.clientY - r.top}px`)
  }
  node.addEventListener('pointermove', onMove)
  return {
    destroy() {
      node.removeEventListener('pointermove', onMove)
    },
  }
}

/** Svelte action: focus the node when it mounts, and select what is in it.
 *
 * For a field that replaces a label in place: the click that opened it is the
 * user asking to type, so the caret has to be there already, and the text it was
 * prefilled with is what they are most likely replacing wholesale. */
export function autofocus(node: HTMLInputElement) {
  node.focus()
  node.select()
}
