<script lang="ts">
  import type { Column } from './codec'

  let { data, schemaBytes, hovered }: { data: Uint8Array; schemaBytes: number; hovered: Column | undefined } =
    $props()

  // Enough to read, not so much that a 200-record message renders 30k spans.
  const LIMIT = 1024
  const shown = $derived(data.subarray(0, LIMIT))

  const rows = $derived(
    Array.from({ length: Math.ceil(shown.length / 16) }, (_, r) => ({
      offset: r * 16,
      cells: Array.from(shown.subarray(r * 16, r * 16 + 16)).map((b, i) => ({
        hex: b.toString(16).padStart(2, '0'),
        at: r * 16 + i,
      })),
    }))
  )

  function kind(at: number): string {
    if (hovered && at >= hovered.start && at < hovered.end) return 'hit'
    if (at === 0) return 'version'
    if (at < schemaBytes) return 'schema'
    return ''
  }
</script>

<div class="hex">
  {#each rows as row (row.offset)}
    <div class="line">
      <span class="offset">{row.offset.toString(16).padStart(4, '0')}</span>
      {#each row.cells as cell (cell.at)}
        <span class="cell {kind(cell.at)}">{cell.hex}</span>
      {/each}
    </div>
  {/each}
  {#if data.length > LIMIT}
    <p class="more">…{data.length - LIMIT} more bytes</p>
  {/if}
</div>

<style>
  .hex {
    font-family: var(--mono);
    font-size: 12px;
    line-height: 1.6;
    overflow: auto;
    max-height: 260px;
  }

  .line {
    white-space: nowrap;
  }

  .offset {
    color: #4a5163;
    margin-right: 10px;
    user-select: none;
  }

  .cell {
    padding: 0 1px;
    color: var(--dim);
  }

  /* The version byte and the schema section, so the framing is visible before
     any column is hovered. */
  .cell.version {
    color: var(--warn);
    font-weight: 600;
  }

  .cell.schema {
    color: #6b7488;
  }

  .cell.hit {
    background: var(--accent);
    color: #0f1117;
    border-radius: 2px;
  }

  .more {
    color: var(--dim);
    margin: 6px 0 0;
  }
</style>
