<script lang="ts">
  import type { Field } from './codec'

  let { data, schemaBytes, hovered }: { data: Uint8Array; schemaBytes: number; hovered: Field | undefined } =
    $props()

  // Enough to read, not so much that a 200-record message renders 30k spans.
  const LIMIT = 1024

  // The dump sits beside the column list, so how many bytes fit on a line is a
  // property of the space it got, not a constant. 16 is the habit; 8 keeps it
  // readable when the pane is halved rather than making it scroll sideways.
  let width = $state(0)
  const CELL = 17 // one "xx" cell at 12px in the mono face, measured
  const GUTTER = 46 // the offset column, its margin, and a little slack
  const fitted = $derived([32, 16, 8].find((n) => width - GUTTER >= n * CELL))
  // Eight to a line is the floor; below that the offsets go rather than the
  // bytes, since what this view is for is seeing a hovered column light up.
  const perRow = $derived(fitted ?? 8)
  const showOffsets = $derived(fitted !== undefined)

  // The window follows the hover. A 50 KB message shows its first kilobyte by
  // default, and every column past that would light up nothing at all — which,
  // beside a list inviting the hover, would read as the highlight being broken.
  const from = $derived(
    !hovered || hovered.start < LIMIT ? 0 : Math.floor(hovered.start / perRow) * perRow
  )
  const shown = $derived(data.subarray(from, from + LIMIT))

  const rows = $derived(
    Array.from({ length: Math.ceil(shown.length / perRow) }, (_, r) => ({
      offset: from + r * perRow,
      cells: Array.from(shown.subarray(r * perRow, r * perRow + perRow)).map((b, i) => ({
        hex: b.toString(16).padStart(2, '0'),
        at: from + r * perRow + i,
      })),
    }))
  )

  function kind(at: number): string {
    if (hovered && at >= hovered.start && at < hovered.end) return 'hit'
    if (at === 0) return 'version'
    if (at < schemaBytes) return 'schema'
    return ''
  }

  // …and scrolls to it, since the box shows about twenty lines of the thousand
  // a large message has.
  let box: HTMLDivElement | undefined = $state()
  $effect(() => {
    if (!hovered || !box) return
    const line = box.querySelector('.line') as HTMLElement | null
    if (!line) return
    const row = Math.floor((hovered.start - from) / perRow)
    box.scrollTop = Math.max(0, row * line.offsetHeight - box.clientHeight / 3)
  })
</script>

<div class="hex" bind:this={box} bind:clientWidth={width}>
  {#each rows as row (row.offset)}
    <div class="line">
      {#if showOffsets}
        <span class="offset">{row.offset.toString(16).padStart(4, '0')}</span>
      {/if}
      {#each row.cells as cell (cell.at)}
        <span class="cell {kind(cell.at)}">{cell.hex}</span>
      {/each}
    </div>
  {/each}
</div>
{#if data.length > shown.length}
  <!-- Outside the scroller, or it sits a thousand lines down where the reader
       who needs it will never look. -->
  <p class="more">
    showing {shown.length} of {data.length} bytes, from {from.toString(16).padStart(4, '0')}
  </p>
{/if}

<style>
  .hex {
    font-family: var(--mono);
    font-size: 12px;
    line-height: 1.6;
    overflow: auto;
    max-height: min(300px, 45dvh);
    /* The lines are wider than a phone; let the dump scroll sideways on its
       own rather than dragging the page with it. */
    overscroll-behavior-x: contain;
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
    font-size: 12px;
    color: var(--dim);
    margin: 6px 0 0;
  }
</style>
