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
</script>

<ul class="tree" style="--depth: {depth}">
  {#each columns as column (column.name + column.start)}
    <li>
      <!-- Hover is the link to the hex view: the bytes highlight there. -->
      <div
        class="row"
        class:is-hovered={hovered === column}
        role="presentation"
        onmouseenter={() => (hovered = column)}
        onmouseleave={() => (hovered = undefined)}
      >
        <span class="name">{column.name}</span>
        <span class="type">
          {column.type}{#if column.nullable}<span class="nullable" title="nullable: a null flag, and a presence bitmap if any value is null">?</span>{/if}
        </span>
        <span class="bytes">{bytes(column.bytes)}</span>
        <span class="share">
          <span class="bar" style="width: {total > 0 ? (column.bytes / total) * 100 : 0}%"></span>
          <span class="pct">{total > 0 ? Math.round((column.bytes / total) * 100) : 0}%</span>
        </span>
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
    padding: 0 0 0 calc(var(--depth) * 0px);
  }

  li {
    margin: 0;
  }

  .row {
    display: grid;
    grid-template-columns: 1fr auto 62px 120px;
    gap: 8px;
    align-items: center;
    padding: 3px 6px 3px calc(6px + var(--depth) * 14px);
    border-radius: 4px;
    cursor: default;
  }

  .row.is-hovered {
    background: #232838;
  }

  .name {
    font-family: var(--mono);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .type {
    font-family: var(--mono);
    font-size: 12px;
    color: var(--dim);
  }

  .nullable {
    color: var(--warn);
  }

  .bytes {
    text-align: right;
    font-variant-numeric: tabular-nums;
    color: var(--dim);
    font-size: 12px;
  }

  .share {
    display: flex;
    align-items: center;
    gap: 6px;
  }

  .bar {
    height: 6px;
    min-width: 1px;
    background: var(--accent);
    border-radius: 3px;
    opacity: 0.7;
  }

  .pct {
    font-size: 11px;
    color: var(--dim);
    font-variant-numeric: tabular-nums;
    margin-left: auto;
  }
</style>
