<script lang="ts">
  import '../app.css'
  const { children } = $props()
</script>

<header>
  <span class="brand">colbin</span>
  <span class="tagline">high performant columnar binary format. JSON compatible CBOR alternative</span>
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

  /* The tagline is a sentence, not a label: on a narrow screen it takes the
     second line whole rather than squeezing the brand and the link out. */
  @media (max-width: 860px) {
    header {
      flex-wrap: wrap;
      gap: 4px 10px;
      padding: 10px 12px;
    }

    .tagline {
      order: 1;
      flex-basis: 100%;
      font-size: 12px;
    }
  }
</style>