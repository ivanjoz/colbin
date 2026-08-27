<script lang="ts">
  import type { Snippet } from 'svelte'
  import { bytes, ratio } from './format'

  let {
    jsonBytes,
    messageBytes,
    jsonGzip,
    messageGzip,
    timing = '',
    showGzip = $bindable(),
    children,
  }: {
    jsonBytes: number
    messageBytes: number
    jsonGzip: number
    messageGzip: number
    timing?: string
    showGzip: boolean
    children?: Snippet
  } = $props()

  // Raw is the default; gzip *replaces* it rather than adding a second pair of
  // bars, because it is the same comparison seen through a compressing
  // transport — one reading of one thing, not two facts to hold at once.
  const left = $derived(showGzip ? jsonGzip : jsonBytes)
  const right = $derived(showGzip ? messageGzip : messageBytes)
  const widest = $derived(Math.max(left, right, 1))
  const wins = $derived(right < left)
</script>

<div class="sizes">
  <div class="headline" class:is-loss={!wins}>
    <strong>{ratio(left, right)}</strong>
    <span>{bytes(left)} → {bytes(right)}</span>
    {#if !wins}
      <em>colbin is larger here</em>
    {/if}
    <span class="timing">{timing}</span>
  </div>

  <div class="bar-row">
    <span class="label">JSON{showGzip ? ' gz' : ''}</span>
    <span class="track"><span class="fill json" style="width: {(left / widest) * 100}%"></span></span>
    <span class="value">{bytes(left)}</span>
  </div>
  <div class="bar-row">
    <span class="label">colbin{showGzip ? ' gz' : ''}</span>
    <span class="track"><span class="fill cb" style="width: {(right / widest) * 100}%"></span></span>
    <span class="value">{bytes(right)}</span>
  </div>

  <div class="meta-row">
    <div class="facts">{@render children?.()}</div>
    <label class="gzip-toggle">
      <input type="checkbox" bind:checked={showGzip} />
      compare gzipped
    </label>
  </div>

  {#if showGzip}
    <p class="gzip-note">
      Both sides compress, so this says whether the saving survives a compressing transport — not
      that gzipped JSON is the thing to beat. It inflates anything under about 100 bytes.
    </p>
  {/if}
</div>

<style>
  .sizes {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .headline {
    display: flex;
    align-items: baseline;
    flex-wrap: wrap;
    gap: 4px 10px;
    margin-bottom: 4px;
  }

  .headline strong {
    font-size: 26px;
    color: var(--good);
    font-variant-numeric: tabular-nums;
  }

  .headline.is-loss strong {
    color: var(--warn);
  }

  .headline span {
    color: var(--dim);
    white-space: nowrap;
  }

  .headline em {
    color: var(--warn);
    font-style: normal;
    font-size: 12px;
  }

  /* Pushed to the end of the headline row: the encode cost belongs with the
     size it bought, and it saves the pane a header of its own. */
  .timing {
    margin-left: auto;
    font-size: 12px;
    font-variant-numeric: tabular-nums;
  }

  .bar-row {
    display: grid;
    grid-template-columns: 56px 1fr 72px;
    gap: 8px;
    align-items: center;
  }

  .label {
    font-size: 12px;
    color: var(--dim);
  }

  .track {
    height: 10px;
    background: #12141c;
    border-radius: 5px;
    overflow: hidden;
  }

  .fill {
    display: block;
    height: 100%;
    border-radius: 5px;
    min-width: 2px;
  }

  .fill.json {
    background: #4b566b;
  }

  .fill.cb {
    background: var(--accent);
  }

  .value {
    text-align: right;
    font-size: 12px;
    font-variant-numeric: tabular-nums;
    color: var(--dim);
  }

  .meta-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    flex-wrap: wrap;
    gap: 4px 12px;
    margin-top: 4px;
  }

  .facts {
    display: flex;
    flex-wrap: wrap;
    gap: 4px 14px;
    font-size: 12px;
    color: var(--dim);
    min-width: 0;
  }

  .gzip-toggle {
    display: flex;
    align-items: center;
    gap: 6px;
    flex: none;
    font-size: 12px;
    color: var(--dim);
    cursor: pointer;
  }

  .gzip-note {
    margin: 2px 0 0;
    font-size: 11px;
    color: var(--dim);
    line-height: 1.5;
  }
</style>
