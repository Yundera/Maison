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

/** Svelte action: focus the node when it mounts, and select what is in it.
 *
 * For a field that replaces a label in place: the click that opened it is the
 * user asking to type, so the caret has to be there already, and the text it was
 * prefilled with is what they are most likely replacing wholesale. */
export function autofocus(node: HTMLInputElement) {
  node.focus()
  node.select()
}
