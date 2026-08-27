<script lang="ts">
  import type { Diagnostic } from './codec'

  let { error, warnings }: { error: Diagnostic | undefined; warnings: string[] } = $props()
</script>

{#if error}
  <div class="box bad">
    <div class="head">
      <strong>Refused</strong>
      {#if error.path}<code>{error.path}</code>{/if}
      {#if error.line > 0}<span class="at">line {error.line}</span>{/if}
    </div>
    <p>{error.message}</p>
  </div>
{/if}

{#each warnings as warning (warning)}
  <div class="box warn">
    <div class="head"><strong>Note</strong></div>
    <p>{warning}</p>
  </div>
{/each}

<style>
  .box {
    border-radius: 6px;
    padding: 10px 12px;
    margin-bottom: 8px;
    border: 1px solid transparent;
  }

  .box.bad {
    background: #2a1b1e;
    border-color: #52323a;
  }

  .box.warn {
    background: #2a2519;
    border-color: #4d4327;
  }

  .head {
    display: flex;
    align-items: baseline;
    gap: 8px;
    margin-bottom: 4px;
  }

  .bad strong {
    color: var(--bad);
  }

  .warn strong {
    color: var(--warn);
  }

  code {
    font-size: 12px;
    color: var(--text);
    background: #00000033;
    padding: 1px 5px;
    border-radius: 3px;
  }

  .at {
    font-size: 12px;
    color: var(--dim);
  }

  p {
    margin: 0;
    line-height: 1.5;
  }
</style>
