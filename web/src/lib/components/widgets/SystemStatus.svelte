<script lang="ts">
  import Widget from '../Widget.svelte'
  import RadialBar from '../RadialBar.svelte'
  import { systemStats } from '../../stores/system'
  import { monitorOpen } from '../../stores/ui'
  import { renderSize } from '../../format'
  import { t } from '../../i18n'

  let useFahrenheit = $state(false)

  const s = $derived($systemStats)
  const tempLabel = $derived(
    s
      ? useFahrenheit
        ? `${Math.round((s.cpu_temp_c * 9) / 5 + 32)}°F`
        : `${Math.round(s.cpu_temp_c)}°C`
      : '0°C',
  )
</script>

<Widget title={$t('system_status')} arrow onarrowclick={() => monitorOpen.set(true)}>
  <div class="gauges">
    <RadialBar
      percent={s?.cpu_percent ?? 0}
      label="CPU"
      extendContent={tempLabel}
      extendClickable
      onextendclick={() => (useFahrenheit = !useFahrenheit)}
    />
    <RadialBar
      percent={s?.mem_percent ?? 0}
      label="RAM"
      extendContent={s ? renderSize(s.mem_total) : ''}
    />
  </div>
</Widget>

<style>
  .gauges {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 0.5rem;
    padding-top: 0.25rem;
  }
</style>
