<script lang="ts">
  import { bytes, ratio } from './format'

  let {
    jsonBytes,
    messageBytes,
    jsonGzip,
    messageGzip,
    showGzip = $bindable(),
  }: {
    jsonBytes: number
    messageBytes: number
    jsonGzip: number
    messageGzip: number
    showGzip: boolean
  } = $props()

  const widest = $derived(Math.max(jsonBytes, messageBytes, 1))
  const wins = $derived(messageBytes < jsonBytes)
</script>

<div class="sizes">
  <div class="headline" class:is-loss={!wins}>
    <strong>{ratio(jsonBytes, messageBytes)}</strong>
    <span>{bytes(jsonBytes)} → {bytes(messageBytes)}</span>
    {#if !wins}
      <em>colbin is larger here</em>
    {/if}
  </div>

  <div class="bar-row">
    <span class="label">JSON</span>
    <span class="track"><span class="fill json" style="width: {(jsonBytes / widest) * 100}%"></span></span>
    <span class="value">{bytes(jsonBytes)}</span>
  </div>
  <div class="bar-row">
    <span class="label">colbin</span>
    <span class="track"><span class="fill cb" style="width: {(messageBytes / widest) * 100}%"></span></span>
    <span class="value">{bytes(messageBytes)}</span>
  </div>

  <!-- Raw is the headline; gzip is a check that the win is information and not
       just locality, and it inflates anything under about 100 bytes. -->
  <label class="gzip-toggle">
    <input type="checkbox" bind:checked={showGzip} />
    compare gzipped
  </label>
  {#if showGzip}
    <div class="bar-row">
      <span class="label">JSON gz</span>
      <span class="track"
        ><span class="fill json dim" style="width: {(jsonGzip / widest) * 100}%"></span></span
      >
      <span class="value">{bytes(jsonGzip)}</span>
    </div>
    <div class="bar-row">
      <span class="label">colbin gz</span>
      <span class="track"><span class="fill cb dim" style="width: {(messageGzip / widest) * 100}%"></span></span>
      <span class="value">{bytes(messageGzip)}</span>
    </div>
    <p class="gzip-note">
      {ratio(jsonGzip, messageGzip)} gzipped. Both sides compress, so this says whether the
      saving survives a compressing transport — not that gzipped JSON is the thing to beat.
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
    gap: 10px;
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
  }

  .headline em {
    color: var(--warn);
    font-style: normal;
    font-size: 12px;
  }

  .bar-row {
    display: grid;
    grid-template-columns: 66px 1fr 72px;
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

  .fill.dim {
    opacity: 0.45;
  }

  .value {
    text-align: right;
    font-size: 12px;
    font-variant-numeric: tabular-nums;
    color: var(--dim);
  }

  .gzip-toggle {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    color: var(--dim);
    margin-top: 4px;
    cursor: pointer;
  }

  .gzip-note {
    margin: 2px 0 0;
    font-size: 11px;
    color: var(--dim);
    line-height: 1.5;
  }
</style>
