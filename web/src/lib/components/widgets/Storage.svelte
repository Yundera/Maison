<script lang="ts">
  import Widget from '../Widget.svelte'
  import { systemStats } from '../../stores/system'
  import { renderSize } from '../../format'
  import { t } from '../../i18n'

  const s = $derived($systemStats)
  const healthy = $derived((s?.disk_percent ?? 0) < 90)
</script>

<Widget title={$t('storage')}>
  <div class="storage">
    <img class="disk" src="/img/storage.svg" alt="" aria-hidden="true" />
    <div class="detail">
      <span class="badge" class:warn={!healthy}>{healthy ? $t('healthy') : $t('almost_full')}</span>
      <div class="lines">
        <span>{$t('used')}: {s ? renderSize(s.disk_used) : '—'}</span>
        <span>{$t('total')}: {s ? renderSize(s.disk_total) : '—'}</span>
      </div>
      <div class="bar"><div class="fill" style:width={`${s?.disk_percent ?? 0}%`}></div></div>
    </div>
  </div>
</Widget>

<style>
  .storage {
    display: flex;
    gap: 0.75rem;
    align-items: center;
  }
  .disk {
    width: 64px;
    height: 64px;
    flex: none;
  }
  .detail {
    flex: 1;
    min-width: 0;
  }
  .badge {
    display: inline-block;
    font-size: 0.7rem;
    font-weight: 600;
    color: var(--green);
    border: 1px solid var(--green);
    border-radius: 4px;
    padding: 0 0.35rem;
    line-height: 1.1rem;
  }
  .badge.warn {
    color: var(--orange);
    border-color: var(--orange);
  }
  .lines {
    display: flex;
    justify-content: space-between;
    gap: 0.5rem;
    margin: 0.4rem 0;
    font-size: 0.8rem;
    color: var(--grey-200);
  }
  .bar {
    height: 4px;
    border-radius: 2px;
    background: rgba(255, 255, 255, 0.25);
    overflow: hidden;
  }
  .fill {
    height: 100%;
    background: var(--primary);
    border-radius: 2px;
    transition: width 0.6s ease;
  }
</style>
