// The JSON text sink: what the walk puts its findings into.
//
// Port of codec/jsontext.go. The output is what encoding/json would have written
// for the same record, down to the escaping and the spelling of numbers — which
// is a stronger claim than "valid JSON", and the one the vectors check, because
// a test that compares parsed values would not notice a float printed one digit
// differently.
//
// The walk hands strings over as UTF-8 byte views onto the message, and the
// escaper works a byte at a time, so nothing here goes through an AssemblyScript
// string on the way out. That is not only speed: a round trip through UTF-16
// would turn an invalid byte into a replacement character before the escaper
// could see it, and matching encoding/json means deciding that case here.

import { Writer } from './bytes'

const HEX_DIGITS = '0123456789abcdef'

/**
 * The two runes that are a line break in JavaScript and not in JSON. They are
 * escaped for the same reason the HTML-significant bytes are: so the output can
 * be pasted into a script tag and still be the document it was.
 */
const LINE_SEPARATOR: i32 = 0x2028
const PARAGRAPH_SEPARATOR: i32 = 0x2029

export class JSONSink {
  out: Writer = new Writer(1024)
  /** Whether a value has been written at the current depth, so a comma goes in
   * front of the next one. */
  private first: Array<bool> = new Array<bool>()
  /** Set when a key has just been written, so the value it belongs to does not
   * also write a comma. */
  private afterKey: bool = false
  error: string = ''

  @inline get ok(): bool {
    return this.error.length == 0
  }

  fail(message: string): void {
    if (this.ok) this.error = message
  }

  private byte(b: u8): void {
    this.out.writeByte(b)
  }

  private ascii(text: string): void {
    for (let index = 0; index < text.length; index++) {
      this.out.writeByte(<u8>text.charCodeAt(index))
    }
  }

  private beforeValue(): void {
    if (this.afterKey) {
      this.afterKey = false
      return
    }
    const depth = this.first.length
    if (depth == 0) return
    if (unchecked(this.first[depth - 1])) {
      this.first[depth - 1] = false
      return
    }
    this.byte(0x2c /* , */)
  }

  private push(): void {
    this.first.push(true)
  }

  private pop(): void {
    if (this.first.length > 0) this.first.pop()
  }

  beginObject(): void {
    this.beforeValue()
    this.byte(0x7b /* { */)
    this.push()
  }

  endObject(): void {
    this.pop()
    this.byte(0x7d /* } */)
  }

  beginArray(): void {
    this.beforeValue()
    this.byte(0x5b /* [ */)
    this.push()
  }

  endArray(): void {
    this.pop()
    this.byte(0x5d /* ] */)
  }

  /** An object key. The value that follows must not write its own comma. */
  key(name: string): void {
    this.beforeValue()
    this.utf8String(String.UTF8.encode(name))
    this.byte(0x3a /* : */)
    this.afterKey = true
  }

  /** An object key already held as UTF-8 bytes. */
  keyBytes(name: Uint8Array): void {
    this.beforeValue()
    this.jsonString(name)
    this.byte(0x3a /* : */)
    this.afterKey = true
  }

  /**
   * A key whose quotes, escaping and colon were all rendered once, ahead of the
   * walk — so a field costs one `memory.copy` per row instead of an encode, two
   * allocations and a byte-at-a-time escape.
   *
   * The names are fixed by the plan and every row repeats them, which is what
   * makes this worth precomputing: on a thousand seven-field records the old
   * path escaped seven distinct strings seven thousand times. `plan.ts` records
   * the same lesson being learned on the encoding side.
   */
  keyRun(run: Uint8Array): void {
    this.beforeValue()
    this.out.writeBytes(run, 0, run.length)
    this.afterKey = true
  }

  null(): void {
    this.beforeValue()
    this.ascii('null')
  }

  boolean(value: bool): void {
    this.beforeValue()
    this.ascii(value ? 'true' : 'false')
  }

  signed(value: i64): void {
    this.beforeValue()
    this.out.writeDecimalI64(value)
  }

  unsigned(value: u64): void {
    this.beforeValue()
    this.out.writeDecimalU64(value)
  }

  /**
   * A float, refused before it is written — so a message with a NaN three fields
   * in leaves the buffer as it found it rather than half a document.
   *
   * JSON has no spelling for a NaN or an infinity. Writing null instead turns
   * "not a number" into "no value", and the two are not the same thing.
   */
  float(value: f64, width: i32): void {
    if (isNaN(value) || !isFinite(value)) {
      this.fail('a NaN or an infinity has no JSON spelling')
      return
    }
    this.beforeValue()
    this.ascii(formatJSONFloat(value, width))
  }

  /** A string held as UTF-8 bytes, which is how the walk holds every one. */
  textBytes(value: Uint8Array): void {
    this.beforeValue()
    this.jsonString(value)
  }

  /**
   * A string that was expanded into `scratch` rather than found in the message.
   *
   * A packed string has no bytes on the wire to hand back a view of — it is a
   * unit stream, and the characters only exist once they are unpacked. So the
   * decoder unpacks into a scratch buffer and this reads the span back out of
   * it, which keeps the one copy that is genuinely needed and adds none.
   */
  textScratch(scratch: Writer, from: i32): void {
    this.beforeValue()
    this.jsonString(scratch.buf.subarray(from, scratch.len))
  }

  text(value: string): void {
    this.beforeValue()
    this.utf8String(String.UTF8.encode(value))
  }

  private utf8String(value: ArrayBuffer): void {
    this.jsonString(Uint8Array.wrap(value))
  }

  /**
   * A []byte as base64 in a string, which is what encoding/json does and
   * therefore what a caller on the other end already has a decoder for. Worth
   * saying out loud because colbin's opBytes and opStrings are different ops and
   * only this one is base64.
   */
  blob(value: Uint8Array): void {
    this.beforeValue()
    this.byte(0x22 /* " */)
    this.base64(value)
    this.byte(0x22)
  }

  private base64(value: Uint8Array): void {
    const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/'
    let at = 0
    while (at + 3 <= value.length) {
      const group =
        (<u32>unchecked(value[at]) << 16) |
        (<u32>unchecked(value[at + 1]) << 8) |
        <u32>unchecked(value[at + 2])
      this.byte(<u8>alphabet.charCodeAt(<i32>(group >> 18) & 0x3f))
      this.byte(<u8>alphabet.charCodeAt(<i32>(group >> 12) & 0x3f))
      this.byte(<u8>alphabet.charCodeAt(<i32>(group >> 6) & 0x3f))
      this.byte(<u8>alphabet.charCodeAt(<i32>group & 0x3f))
      at += 3
    }
    const left = value.length - at
    if (left == 0) return
    let group = <u32>unchecked(value[at]) << 16
    if (left == 2) group |= <u32>unchecked(value[at + 1]) << 8
    this.byte(<u8>alphabet.charCodeAt(<i32>(group >> 18) & 0x3f))
    this.byte(<u8>alphabet.charCodeAt(<i32>(group >> 12) & 0x3f))
    if (left == 2) {
      this.byte(<u8>alphabet.charCodeAt(<i32>(group >> 6) & 0x3f))
    } else {
      this.byte(0x3d /* = */)
    }
    this.byte(0x3d)
  }

  /** A quoted, escaped JSON string, with encoding/json's default escape set. */
  private jsonString(value: Uint8Array): void {
    this.byte(0x22 /* " */)
    let start = 0
    let at = 0
    while (at < value.length) {
      const character = unchecked(value[at])
      if (character < 0x80) {
        if (jsonSafe(character)) {
          at++
          continue
        }
        this.out.writeBytes(value, start, at - start)
        if (character == 0x5c /* \ */ || character == 0x22 /* " */) {
          this.byte(0x5c)
          this.byte(character)
        } else if (character == 0x0a) {
          this.ascii('\\n')
        } else if (character == 0x0d) {
          this.ascii('\\r')
        } else if (character == 0x09) {
          this.ascii('\\t')
        } else {
          // A control byte, or one of the three HTML-significant ones, which go
          // the long way round for the same reason: so the output is safe
          // wherever it is pasted.
          this.ascii('\\u00')
          this.byte(<u8>HEX_DIGITS.charCodeAt(<i32>(character >> 4)))
          this.byte(<u8>HEX_DIGITS.charCodeAt(<i32>(character & 0xf)))
        }
        at++
        start = at
        continue
      }
      const width = runeWidth(value, at)
      if (width == 1) {
        // Invalid UTF-8 is replaced rather than refused, which is what
        // encoding/json does and what keeps one bad byte from losing a whole
        // message.
        this.out.writeBytes(value, start, at - start)
        this.ascii('\\ufffd')
      } else {
        const rune = decodeRune(value, at, width)
        if (rune == LINE_SEPARATOR || rune == PARAGRAPH_SEPARATOR) {
          this.out.writeBytes(value, start, at - start)
          this.ascii('\\u202')
          this.byte(<u8>HEX_DIGITS.charCodeAt(rune & 0xf))
        } else {
          at += width
          continue
        }
      }
      at += width
      start = at
    }
    this.out.writeBytes(value, start, value.length - start)
    this.byte(0x22)
  }

  take(): Uint8Array {
    return this.out.take()
  }
}

/**
 * `"name":` — quoted, escaped and punctuated — for `JSONSink#keyRun`.
 *
 * Rendered through a throwaway sink rather than a second copy of the escaper, so
 * a precomputed key cannot drift from the one the walker would have written.
 */
export function renderKeyRun(name: Uint8Array): Uint8Array {
  const sink = new JSONSink()
  sink.keyBytes(name)
  return sink.take()
}

/** Whether a byte may go into a string as it stands. */
@inline
function jsonSafe(character: u8): bool {
  return (
    character >= 0x20 &&
    character != 0x22 /* " */ &&
    character != 0x5c /* \ */ &&
    character != 0x3c /* < */ &&
    character != 0x3e /* > */ &&
    character != 0x26 /* & */
  )
}

/**
 * How many bytes the rune at `at` occupies, or 1 when the sequence is not valid
 * UTF-8 — which is how Go's utf8.DecodeRune reports one, and the distinction the
 * escaper turns into U+FFFD.
 */
function runeWidth(buf: Uint8Array, at: i32): i32 {
  const first = unchecked(buf[at])
  let width = 0
  let lowest: i32 = 0
  if (first >= 0xc2 && first <= 0xdf) {
    width = 2
    lowest = 0x80
  } else if (first >= 0xe0 && first <= 0xef) {
    width = 3
    lowest = 0x800
  } else if (first >= 0xf0 && first <= 0xf4) {
    width = 4
    lowest = 0x10000
  } else {
    return 1
  }
  if (at + width > buf.length) return 1
  for (let index = 1; index < width; index++) {
    const b = unchecked(buf[at + index])
    if (b < 0x80 || b > 0xbf) return 1
  }
  const rune = decodeRune(buf, at, width)
  // Overlong forms, surrogates and anything past U+10FFFF are not valid UTF-8,
  // and Go reports each as a one-byte error rather than decoding it.
  if (rune < lowest || rune > 0x10ffff || (rune >= 0xd800 && rune <= 0xdfff)) return 1
  return width
}

@inline
function decodeRune(buf: Uint8Array, at: i32, width: i32): i32 {
  if (width == 2) {
    return ((<i32>unchecked(buf[at]) & 0x1f) << 6) | (<i32>unchecked(buf[at + 1]) & 0x3f)
  }
  if (width == 3) {
    return (
      ((<i32>unchecked(buf[at]) & 0x0f) << 12) |
      ((<i32>unchecked(buf[at + 1]) & 0x3f) << 6) |
      (<i32>unchecked(buf[at + 2]) & 0x3f)
    )
  }
  return (
    ((<i32>unchecked(buf[at]) & 0x07) << 18) |
    ((<i32>unchecked(buf[at + 1]) & 0x3f) << 12) |
    ((<i32>unchecked(buf[at + 2]) & 0x3f) << 6) |
    (<i32>unchecked(buf[at + 3]) & 0x3f)
  )
}

/**
 * A float the way encoding/json writes one: 'f' notation in the range a reader
 * expects to see it, 'e' outside it, and in either case the shortest form that
 * reads back as the same value.
 *
 * The thresholds encoding/json uses — below 1e-6 or at least 1e21 — are the same
 * ones JavaScript's own number formatting uses, and AssemblyScript's toString
 * implements JavaScript's. So the format selection, the shortest-round-trip
 * digits and the exponent's spelling all come out right by themselves, measured
 * against JavaScript across the corpus. Three differences are left, each a real
 * one:
 *
 *   - **a trailing `.0`.** AssemblyScript writes `1.0` where both Go and
 *     JavaScript write `1`. Only in 'f' notation, and only when every digit
 *     after the point is that one zero, so trimming it is exact rather than a
 *     guess — `1.05` and `1e+21` are untouched.
 *   - **negative zero.** Go writes `-0`; AssemblyScript writes `0.0`.
 *   - **width.** A float32 must be formatted at 32 bits, or `1.1` prints as
 *     1.100000023841858.
 */
export function formatJSONFloat(value: f64, width: i32): string {
  if (value == 0) {
    // The sign of a zero survives the comparison, so `-0` is not `0`. Go writes
    // both, and losing the sign would lose the difference.
    return 1 / value < 0 ? '-0' : '0'
  }
  const text = width == 32 ? (<f32>value).toString() : value.toString()
  const end = text.length
  if (end >= 3 && text.charCodeAt(end - 2) == 0x2e /* . */ && text.charCodeAt(end - 1) == 0x30) {
    return text.substring(0, end - 2)
  }
  return text
}
