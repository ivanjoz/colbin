<script lang="ts">
  import type { Column } from './codec'
  import { bytes } from './format'
  // Self-import rather than <svelte:self>, which Svelte 5 deprecates.
  import ColumnTree from './ColumnTree.svelte'

  let {
    columns,
    total,
    depth = 0,
    hovered = $bindable(),
  }: {
    columns: Column[]
    total: number
    depth?: number
    hovered: Column | undefined
  } = $props()

  const share = (column: Column) => (total > 0 ? (column.bytes / total) * 100 : 0)

  // The type and the exact share moved into the title: a row is read as "which
  // field is the message", and a column of int64/string/bool repeated down the
  // side answers a question nobody asked first.
  const detail = (column: Column) =>
    `${column.type}${column.nullable ? ' · nullable' : ''} · ${share(column).toFixed(1)}% of the message`
</script>

<ul class="tree" style="margin-left: {depth > 0 ? 12 : 0}px">
  {#each columns as column (column.name + column.start)}
    <li>
      <!-- Hover is the link to the hex view: the bytes highlight there. -->
      <div
        class="row"
        class:is-hovered={hovered === column}
        title={detail(column)}
        role="presentation"
        onmouseenter={() => (hovered = column)}
        onmouseleave={() => (hovered = undefined)}
      >
        <span class="fill" style="width: {share(column)}%"></span>
        <span class="name"
          >{column.name}{#if column.nullable}<span class="nullable">?</span>{/if}</span
        >
        <span class="bytes">{bytes(column.bytes)}</span>
      </div>
      {#if column.children.length > 0}
        <ColumnTree columns={column.children} {total} depth={depth + 1} bind:hovered />
      {/if}
    </li>
  {/each}
</ul>

<style>
  .tree {
    list-style: none;
    margin: 0;
    padding: 0;
  }

  /* Nested columns (an array's element, an object's fields) step in as a group,
     so the bars stay one readable stack. The indent is inline because Svelte
     cannot see a recursive component's own nesting to scope `.tree .tree`. */

  li {
    margin: 0 0 3px;
  }

  /* One bar per column: the fill is the share of the message, the name rides
     on top of it, the size sits at the end. */
  .row {
    position: relative;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    height: 24px;
    padding: 0 8px;
    border: 1px solid var(--line);
    border-radius: 4px;
    background: var(--panel-2);
    overflow: hidden;
    cursor: default;
  }

  .fill {
    position: absolute;
    left: 0;
    top: 0;
    bottom: 0;
    min-width: 2px;
    background: var(--accent);
    opacity: 0.55;
  }

  .row.is-hovered {
    border-color: var(--accent);
  }

  .row.is-hovered .fill {
    opacity: 0.85;
  }

  .name,
  .bytes {
    position: relative; /* above the fill */
  }

  .name {
    font-family: var(--mono);
    font-size: 12px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .nullable {
    color: var(--warn);
  }

  .bytes {
    flex: none;
    font-size: 11px;
    font-variant-numeric: tabular-nums;
    color: var(--dim);
  }
</style>
