<script lang="ts">
  import '../app.css'
  const { children } = $props()
</script>

<header>
  <span class="brand">colbin</span>
  <span class="tagline">a columnar binary format for arrays of records — running here as WebAssembly</span>
  <span class="spacer"></span>
  <a href="https://github.com/ivanjoz/colbin" rel="noreferrer">GitHub</a>
</header>

{@render children()}

<style>
  header {
    flex: none;
    display: flex;
    align-items: baseline;
    gap: 12px;
    padding: 10px 16px;
    border-bottom: 1px solid var(--line);
    background: var(--panel);
  }

  .brand {
    font-family: var(--mono);
    font-weight: 700;
    font-size: 16px;
    color: var(--accent);
  }

  .tagline {
    color: var(--dim);
    font-size: 13px;
  }

  .spacer {
    flex: 1;
  }

  a {
    font-size: 13px;
    text-decoration: none;
  }

  a:hover {
    text-decoration: underline;
  }
</style>
