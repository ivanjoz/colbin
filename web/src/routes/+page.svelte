<script lang="ts">
  import { onMount } from 'svelte'
  import { decode, encode, inspect, preload, type Column, type Diagnostic, type Report } from '$lib/codec'
  import { bytes, compactSize, gzipSize } from '$lib/format'
  import { defaultExample, examples, type Example } from '$lib/examples'
  import ColumnTree from '$lib/ColumnTree.svelte'
  import Diagnostics from '$lib/Diagnostics.svelte'
  import HexView from '$lib/HexView.svelte'
  import SizeBars from '$lib/SizeBars.svelte'

  let selected = $state<Example>(defaultExample)
  let text = $state(defaultExample.json)
  let status = $state<'idle' | 'working' | 'ready' | 'failed'>('idle')

  let message = $state<Uint8Array | undefined>()
  let report = $state<Report | undefined>()
  let error = $state<Diagnostic | undefined>()
  let warnings = $state<string[]>([])
  let jsonGzip = $state(0)
  let messageGzip = $state(0)
  let encodeMs = $state(0)

  let hovered = $state<Column | undefined>()
  let showGzip = $state(false)

  // Compared against the compact form: the examples are pretty-printed to be
  // readable, and measuring against that would flatter colbin by two or three
  // times for free.
  const jsonBytes = $derived(compactSize(text))

  onMount(preload)

  function choose(example: Example) {
    selected = example
    text = example.json
  }

  /** Re-encodes on a debounce, so typing does not queue a run per keystroke. */
  let timer: ReturnType<typeof setTimeout> | undefined
  $effect(() => {
    const current = text
    clearTimeout(timer)
    timer = setTimeout(() => void run(current), 180)
    return () => clearTimeout(timer)
  })

  async function run(source: string) {
    status = 'working'
    const started = performance.now()
    const encoded = await encode(source)
    encodeMs = performance.now() - started

    if (!encoded.ok) {
      error = encoded.error
      warnings = []
      message = undefined
      report = undefined
      status = 'failed'
      return
    }

    error = undefined
    warnings = encoded.warnings
    message = encoded.value

    const inspected = await inspect(encoded.value)
    report = inspected.ok ? inspected.value : undefined
    status = 'ready'
    ;[jsonGzip, messageGzip] = await Promise.all([gzipSize(source), gzipSize(encoded.value)])
  }

  function download() {
    if (!message) return
    const blob = new Blob([message as BlobPart], { type: 'application/octet-stream' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `${selected.key}.cbj`
    a.click()
    URL.revokeObjectURL(url)
  }

  async function upload(event: Event) {
    const file = (event.target as HTMLInputElement).files?.[0]
    if (!file) return
    const raw = new Uint8Array(await file.arrayBuffer())
    const back = await decode(raw)
    if (!back.ok) {
      error = back.error
      status = 'failed'
      return
    }
    // Round-tripping the uploaded message back through the editor: whatever it
    // held is now JSON, and re-encoding it is the ordinary path again.
    text = back.value
    selected = { ...selected, key: file.name.replace(/\.[^.]+$/, '') }
  }
</script>

<div class="shell">
  <aside>
    <h2>Examples</h2>
    <ul>
      {#each examples as example (example.key)}
        <li>
          <button class:is-active={selected.key === example.key} onclick={() => choose(example)}>
            {example.title}
          </button>
        </li>
      {/each}
    </ul>
    <p class="hint">
      Every example is editable. The five at the bottom are cases colbin does badly on, or refuses.
    </p>
    <label class="upload">
      <input type="file" accept=".cbj,.cb,application/octet-stream" onchange={upload} />
      <span>Open a .cbj file…</span>
    </label>
  </aside>

  <main>
    <section class="editor">
      <div class="pane-head">
        <h2>JSON</h2>
        <span class="meta" title="With formatting whitespace removed, which is what would actually be sent">
          {bytes(jsonBytes)} minified
        </span>
      </div>
      <textarea bind:value={text} spellcheck="false" aria-label="JSON input"></textarea>
      <p class="note">{selected.note}</p>
    </section>

    <section class="result">
      <div class="scroll">
        <Diagnostics {error} {warnings} />

        {#if status === 'working' && !report}
          <p class="sub">encoding…</p>
        {/if}

        {#if report && message}
          <!-- The ratio is this pane's heading: it says what the pane is for, so
               the pane does not also need a row that says "Message". -->
          <SizeBars
            {jsonBytes}
            messageBytes={message.length}
            {jsonGzip}
            {messageGzip}
            timing={status === 'working' ? 'encoding…' : `${encodeMs.toFixed(1)} ms`}
            bind:showGzip
          >
            <span>{report.recordCount} records</span>
            <span>{report.columns.length} columns</span>
            <span title="The schema section: field names and the type facts the columns leave out. It describes the type, so it does not grow with the record count.">
              schema {bytes(report.schemaBytes)}
            </span>
          </SizeBars>

          <!-- Side by side, because the two halves are one gesture: hover a
               column on the left, its bytes light up on the right. -->
          <div class="panels">
            <div class="panel">
              <h3>Columns</h3>
              <p class="sub">Hover a column to find its bytes in the message.</p>
              <ColumnTree columns={report.columns} total={report.totalBytes} bind:hovered />
            </div>
            <div class="panel">
              <div class="panel-head">
                <h3>Bytes</h3>
                <button class="download" onclick={download}>Download .cbj</button>
              </div>
              <p class="sub">The version byte and schema are dimmed.</p>
              <HexView data={message} schemaBytes={report.schemaBytes} {hovered} />
            </div>
          </div>
        {/if}
      </div>
    </section>
  </main>
</div>

<style>
  .shell {
    display: grid;
    grid-template-columns: 230px 1fr;
    flex: 1;
    min-height: 0;
  }

  aside {
    border-right: 1px solid var(--line);
    background: var(--panel);
    padding: 12px;
    overflow: auto;
  }

  aside h2,
  .pane-head h2 {
    font-size: 11px;
    text-transform: uppercase;
    letter-spacing: 0.08em;
    color: var(--dim);
    margin: 0 0 8px;
  }

  aside ul {
    list-style: none;
    margin: 0;
    padding: 0;
  }

  aside button {
    display: block;
    width: 100%;
    text-align: left;
    background: none;
    border: 0;
    color: var(--text);
    padding: 6px 8px;
    border-radius: 4px;
    cursor: pointer;
    font: inherit;
  }

  aside button:hover {
    background: var(--panel-2);
  }

  aside button.is-active {
    background: #23304a;
    color: #fff;
  }

  .hint {
    font-size: 11px;
    color: var(--dim);
    line-height: 1.5;
    margin: 12px 0;
  }

  .upload input {
    display: none;
  }

  .upload span {
    display: block;
    text-align: center;
    font-size: 14px;
    padding: 7px;
    border: 1px dashed var(--line);
    border-radius: 5px;
    color: var(--dim);
    cursor: pointer;
  }

  .upload span:hover {
    border-color: var(--accent);
    color: var(--accent);
  }

  main {
    display: grid;
    /* The message side carries two panels now, so it gets the wider share. */
    grid-template-columns: minmax(0, 1fr) minmax(0, 1.25fr);
    min-width: 0;
    min-height: 0;
  }

  section {
    display: flex;
    flex-direction: column;
    min-width: 0;
    /* Without this a grid item is at least as tall as its content, and the
       panes push the shell past the viewport instead of scrolling inside. */
    min-height: 0;
    padding: 12px;
  }

  .result {
    border-left: 1px solid var(--line);
    background: var(--panel);
  }

  .pane-head {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
  }

  .meta {
    font-size: 14px;
    color: var(--dim);
    font-variant-numeric: tabular-nums;
  }

  textarea {
    flex: 1;
    min-height: 60px;
    resize: none;
    background: var(--panel-2);
    color: var(--text);
    border: 1px solid var(--line);
    border-radius: 6px;
    padding: 10px;
    font-family: var(--mono);
    font-size: 14px;
    line-height: 1.6;
  }

  textarea:focus {
    outline: 1px solid var(--accent);
  }

  .note {
    font-size: 14px;
    color: var(--dim);
    line-height: 1.55;
    margin: 8px 0 0;
  }

  .scroll {
    overflow: auto;
    flex: 1;
  }

  /* Two columns when there is room, one when there is not — the hex dump drops
     to eight bytes a line rather than scrolling sideways. */
  .panels {
    display: grid;
    margin-top: 22px;
    grid-template-columns: repeat(auto-fit, minmax(170px, 1fr));
    gap: 6px 18px;
    align-items: start;
  }

  .panel {
    min-width: 0;
  }

  h3 {
    font-size: 11px;
    text-transform: uppercase;
    letter-spacing: 0.08em;
    color: var(--dim);
    margin: 0 0 4px;
  }

  .sub {
    font-size: 11px;
    color: var(--dim);
    margin: 0 0 10px;
    line-height: 1.5;
  }

  /* The download sits on the Bytes heading: it is the bytes, in a file. */
  .panel-head {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 8px;
  }

  .download {
    font: inherit;
    font-size: 14px;
    padding: 6px 12px;
    border-radius: 4px;
    border: 0;
    background: var(--accent);
    color: #0f1117;
    cursor: pointer;
    white-space: nowrap;
  }
</style>
