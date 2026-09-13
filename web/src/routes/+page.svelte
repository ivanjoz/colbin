<script lang="ts">
  import { onMount } from 'svelte'
  import {
    decode,
    encode,
    inspect,
    preload,
    type Encoded,
    type Field,
    type Diagnostic,
    type Report,
  } from '$lib/codec'
  import { bytes, compactSize, gzipSize } from '$lib/format'
  import { defaultExample, examples, type Example } from '$lib/examples'
  import ColumnTree from '$lib/ColumnTree.svelte'
  import Diagnostics from '$lib/Diagnostics.svelte'
  import HexView from '$lib/HexView.svelte'
  import SizeBars from '$lib/SizeBars.svelte'

  let selected = $state<Example>(defaultExample)
  let text = $state(defaultExample.json)
  let status = $state<'idle' | 'working' | 'ready' | 'failed'>('idle')

  let encoded = $state<Encoded | undefined>()
  let report = $state<Report | undefined>()
  let error = $state<Diagnostic | undefined>()
  let warnings = $state<string[]>([])
  let jsonGzip = $state(0)
  let messageGzip = $state(0)
  let encodeMs = $state(0)

  let hovered = $state<Field | undefined>()
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
    const result = await encode(source)
    encodeMs = performance.now() - started

    if (!result.ok) {
      error = result.error
      warnings = []
      encoded = undefined
      report = undefined
      status = 'failed'
      return
    }

    error = undefined
    warnings = result.warnings
    encoded = result.value

    const inspected = await inspect(result.value.message, result.value.section)
    report = inspected.ok ? inspected.value : undefined
    status = 'ready'
    ;[jsonGzip, messageGzip] = await Promise.all([
      gzipSize(source),
      gzipSize(result.value.message),
    ])
  }

  // The file gets the self-describing form, not the one the page measures. A
  // message on a wire is sent behind a section the far end already has; a file
  // has nowhere to put one, so it carries its own and the root byte says so.
  function download() {
    if (!encoded) return
    const blob = new Blob([encoded.standalone as BlobPart], { type: 'application/octet-stream' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `${selected.key}.cb`
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
      Every example is editable. You can paste your own JSON payload.
    </p>
    <label class="upload">
      <input type="file" accept=".cb,.cbj,application/octet-stream" onchange={upload} />
      <span>Open a .cb file…</span>
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

        {#if report && encoded}
          <!-- The ratio is this pane's heading: it says what the pane is for, so
               the pane does not also need a row that says "Message". -->
          <SizeBars
            {jsonBytes}
            messageBytes={encoded.message.length}
            {jsonGzip}
            {messageGzip}
            timing={status === 'working' ? 'encoding…' : `${encodeMs.toFixed(1)} ms`}
            bind:showGzip
          >
            <span class="fact">{report.rows} {report.rows === 1 ? 'record' : 'records'}</span>
            <span class="fact">{report.fields.length} {report.fields.length === 1 ? 'field' : 'fields'}</span>
            <span
              class="fact"
              title="Four-bit field ids are the fast path. A type goes to eight when it has more than sixteen fields, which costs a byte per present field."
            >
              {report.wide ? '8-bit keys' : '4-bit keys'}
            </span>
            <span
              class="fact"
              title="The schema section: the field names and the type facts the bytes leave out. It is sent once per connection, not per message, so the bar above measures the body alone."
            >
              schema {bytes(encoded.section.length)} · sent once
            </span>
            <!-- The other delivery, and the one that keeps the bar honest. A
                 stream sends the schema once and the bar is the whole truth; a
                 single file has nowhere to put one and carries its own, which
                 on a small document costs more than the document. Showing both
                 is what stops the headline ratio from being a stream's number
                 quoted at a file. -->
            <span
              class="fact"
              class:is-loss={encoded.standalone.length >= jsonBytes}
              title="The same document as a standalone file: the schema section in front of the body, which is what the download button writes. A small document is mostly schema."
            >
              as one file {bytes(encoded.standalone.length)} ·
              {(jsonBytes / encoded.standalone.length).toFixed(2)}×
            </span>
          </SizeBars>

          <div class="download-row">
            <button class="download" onclick={download}>Download .cb</button>
          </div>

          <!-- Side by side, because the two halves are one gesture: hover a
               column on the left, its bytes light up on the right. -->
          <div class="panels">
            <div class="panel">
              <h3>Fields</h3>
              <p class="sub">Hover or tap a field to find its bytes in the message.</p>
              <ColumnTree columns={report.fields} total={report.totalBytes} bind:hovered />
            </div>
            <div class="panel">
              <h3>Bytes</h3>
              <p class="sub">The root byte is dimmed. Everything after it is the record.</p>
              <HexView data={encoded.message} schemaBytes={report.rootBytes} {hovered} />
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
    font-size: 12px;
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
    font-size: 13px;
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

  /* A ratio under one is the point of the example rather than a fault, so it is
     marked rather than hidden. */
  :global(.fact.is-loss) {
    color: #b45309;
  }

  /* The facts read as one line, so they are separated rather than merely
     spaced. The rule lives here because the spans do: SizeBars renders them
     through a snippet and cannot reach them with a scoped selector. */
  .fact + .fact::before {
    content: '·';
    margin-right: 14px; /* mirrors the flex gap on the other side of the dot */
    opacity: 0.55;
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
    font-size: 12px;
    text-transform: uppercase;
    letter-spacing: 0.08em;
    color: var(--dim);
    margin: 0 0 4px;
  }

  .sub {
    font-size: 12px;
    color: var(--dim);
    margin: 0 0 10px;
    line-height: 1.5;
  }

  /* Its own line, pulled up into the space the panels leave below the facts,
     so it reads as a break between the two rather than costing a whole one. */
  .download-row {
    display: flex;
    justify-content: flex-end;
    margin: 8px 0 -24px 6px ;
  }

  .download {
    font: inherit;
    font-size: 14px;
    padding: 6px 16px;
    border-radius: 4px;
    border: 0;
    background: var(--accent);
    color: #0f1117;
    cursor: pointer;
    white-space: nowrap;
  }

  /* One column below the breakpoint. The sidebar becomes a strip of chips
     across the top — eleven examples stacked would push the editor off the
     screen before the reader had typed anything. */
  @media (max-width: 860px) {
    .shell {
      grid-template-columns: 1fr;
    }

    aside {
      display: grid;
      grid-template-columns: 1fr auto;
      align-items: center;
      gap: 8px;
      overflow: visible;
      padding: 10px 0 10px 12px;
      border-right: 0;
      border-bottom: 1px solid var(--line);
    }

    aside h2 {
      grid-area: 1 / 1;
      margin: 0;
    }

    .upload {
      grid-area: 1 / 2;
      margin-right: 12px;
    }

    .upload span {
      padding: 6px 12px;
      font-size: 13px;
      white-space: nowrap;
    }

    /* The strip scrolls sideways and runs to the edge of the screen, so a
       half-cut chip says there are more of them. */
    aside ul {
      grid-area: 2 / 1 / 3 / 3;
      display: flex;
      gap: 6px;
      overflow-x: auto;
      padding: 2px 12px 2px 0;
      scrollbar-width: none;
    }

    aside ul::-webkit-scrollbar {
      display: none;
    }

    aside li {
      flex: none;
    }

    aside button {
      width: auto;
      white-space: nowrap;
      border: 1px solid var(--line);
      border-radius: 999px;
      padding: 6px 12px;
      font-size: 13px;
    }

    /* The chips already say the examples are switchable, and the note under
       the editor carries the rest. */
    .hint {
      display: none;
    }

    main {
      grid-template-columns: 1fr;
    }

    .result {
      border-left: 0;
      border-top: 1px solid var(--line);
    }

    /* Nothing above it is a fixed height any more, so the editor states its
       own instead of stretching to fill a box that is not there. */
    textarea {
      flex: none;
      height: 38dvh;
      min-height: 200px;
      /* Under 16px, iOS zooms the page when the field takes focus. */
      font-size: 16px;
    }

    .scroll {
      overflow: visible;
    }

    .panels {
      grid-template-columns: 1fr;
      gap: 18px;
      margin-top: 18px;
    }

    /* The pull-up assumed a wide facts row with space to its right. Stacked,
       the button gets its own line. */
    .download-row {
      margin: 12px 0 0;
    }
  }
</style>