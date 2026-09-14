/** Byte counts, sizes and the gzip comparison the page shows. */

/**
 * The size of the JSON with its formatting whitespace removed.
 *
 * The examples are pretty-printed so they can be read and edited, but nobody
 * ships pretty-printed JSON over a wire, and comparing against it would flatter
 * colbin by two or three times. So the comparison is against the compact form.
 *
 * Done with a scanner rather than JSON.parse + stringify because parsing would
 * round any integer past 2^53 -- the exact failure this page exists to show.
 */
export function compactSize(text: string): number {
  const src = new TextEncoder().encode(text)
  let size = 0
  let inString = false
  let escaped = false
  for (let i = 0; i < src.length; i++) {
    const c = src[i]
    if (inString) {
      size++
      if (escaped) escaped = false
      else if (c === 0x5c) escaped = true
      else if (c === 0x22) inString = false
      continue
    }
    if (c === 0x22) {
      inString = true
      size++
      continue
    }
    // Whitespace outside a string is formatting and would not be transmitted.
    if (c === 0x20 || c === 0x09 || c === 0x0a || c === 0x0d) continue
    size++
  }
  return size
}

export function bytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(2)} MB`
}

export function ratio(from: number, to: number): string {
  if (to === 0) return '--'
  return `${(from / to).toFixed(2)}x`
}

/**
 * gzip, through the browser's own compressor.
 *
 * Reported as a secondary number, not the headline: what colbin controls is the
 * raw size, and gzip inflates anything under about 100 bytes.
 * It is here because someone will ask, and because the bytes colbin removes are
 * bytes the compressor no longer has to walk.
 */
export async function gzipSize(data: Uint8Array | string): Promise<number> {
  const input = typeof data === 'string' ? new TextEncoder().encode(compact(data)) : data
  if (typeof CompressionStream === 'undefined') return 0
  const stream = new Blob([input as BlobPart]).stream().pipeThrough(new CompressionStream('gzip'))
  const compressed = await new Response(stream).arrayBuffer()
  return compressed.byteLength
}

/** The text with its formatting whitespace removed, for a fair gzip comparison. */
function compact(text: string): string {
  let out = ''
  let inString = false
  let escaped = false
  for (const ch of text) {
    if (inString) {
      out += ch
      if (escaped) escaped = false
      else if (ch === '\\') escaped = true
      else if (ch === '"') inString = false
      continue
    }
    if (ch === '"') {
      inString = true
      out += ch
      continue
    }
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r') continue
    out += ch
  }
  return out
}

export function hexDump(data: Uint8Array, from: number, to: number): string {
  let out = ''
  for (let i = from; i < to && i < data.length; i++) {
    out += data[i].toString(16).padStart(2, '0')
    out += (i - from) % 16 === 15 ? '\n' : ' '
  }
  return out.trimEnd()
}
