<script lang="ts">
  /**
   * A small filled line chart, drawn as one SVG polyline.
   *
   * No charting library: everything the Resources page plots is a single series
   * against time with no axes, no legend and no interaction, and a dependency for
   * that would be larger than the page it draws on.
   *
   * Points carry their own timestamp so a break in the recording draws as a break.
   * The alternative — plotting by array index — would join the last sample before
   * a reboot to the first one after it with a straight line, inventing a smooth
   * transit across hours when the box was off.
   */

  interface Point {
    /** Unix milliseconds. */
    at: number
    value: number
    /** Optional envelope; when present it is shaded behind the line. */
    min?: number
    max?: number
  }

  let {
    points = [],
    color = 'var(--primary)',
    /** Fixes the top of the scale (100 for a percentage). Otherwise the largest
     *  value in view sets it, which is what a rate needs. */
    max: fixedMax = undefined as number | undefined,
    /** Points further apart than this are treated as separated by a gap. */
    gapMs = 0,
    height = 48,
    label = '',
  }: {
    points?: Point[]
    color?: string
    max?: number
    gapMs?: number
    height?: number
    label?: string
  } = $props()

  const W = 240

  // Segments of consecutive points; a gap starts a new one.
  const segments = $derived.by(() => {
    const out: Point[][] = []
    let cur: Point[] = []
    for (const p of points) {
      if (cur.length && gapMs > 0 && p.at - cur[cur.length - 1].at > gapMs * 1.5) {
        out.push(cur)
        cur = []
      }
      cur.push(p)
    }
    if (cur.length) out.push(cur)
    return out.filter((s) => s.length > 0)
  })

  const from = $derived(points.length ? points[0].at : 0)
  const span = $derived(points.length ? Math.max(1, points[points.length - 1].at - from) : 1)

  const hi = $derived.by(() => {
    if (fixedMax !== undefined) return Math.max(fixedMax, 1e-9)
    let m = 0
    for (const p of points) m = Math.max(m, p.max ?? p.value)
    return Math.max(m, 1e-9)
  })

  const x = (at: number) => ((at - from) / span) * W
  const y = (v: number) => height - Math.min(1, Math.max(0, v / hi)) * height

  function line(seg: Point[]): string {
    return seg.map((p) => `${x(p.at).toFixed(1)},${y(p.value).toFixed(1)}`).join(' ')
  }

  /** The area under the line, closed along the baseline. */
  function area(seg: Point[]): string {
    const first = x(seg[0].at).toFixed(1)
    const last = x(seg[seg.length - 1].at).toFixed(1)
    return `${first},${height} ${line(seg)} ${last},${height}`
  }

  /** The min/max envelope: forward along the maxima, back along the minima. */
  function envelope(seg: Point[]): string {
    const up = seg.map((p) => `${x(p.at).toFixed(1)},${y(p.max ?? p.value).toFixed(1)}`)
    const down = [...seg]
      .reverse()
      .map((p) => `${x(p.at).toFixed(1)},${y(p.min ?? p.value).toFixed(1)}`)
    return [...up, ...down].join(' ')
  }

  const hasEnvelope = $derived(points.some((p) => p.max !== undefined && p.max !== p.min))
</script>

{#if points.length > 1}
  <svg
    viewBox={`0 0 ${W} ${height}`}
    preserveAspectRatio="none"
    style:height={`${height}px`}
    role="img"
    aria-label={label}
  >
    {#each segments as seg, i (i)}
      {#if seg.length > 1}
        <polyline points={area(seg)} fill={color} fill-opacity="0.13" stroke="none" />
        {#if hasEnvelope}
          <polyline points={envelope(seg)} fill={color} fill-opacity="0.16" stroke="none" />
        {/if}
        <polyline
          points={line(seg)}
          fill="none"
          stroke={color}
          stroke-width="1.5"
          stroke-linejoin="round"
          stroke-linecap="round"
          vector-effect="non-scaling-stroke"
        />
      {/if}
    {/each}
  </svg>
{:else}
  <div class="empty" style:height={`${height}px`}></div>
{/if}

<style>
  svg {
    display: block;
    width: 100%;
  }
  .empty {
    width: 100%;
    border-bottom: 1px dashed var(--grey-300);
  }
</style>
