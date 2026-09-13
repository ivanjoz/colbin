// The public ABI (PLAN.md §2.1, REFACTOR_PLAN.md §6).
//
// UTF-8 bytes in, UTF-8 bytes out, and no other shape crosses the boundary: the
// host never hands over a JavaScript object graph, because JSON.parse would have
// already destroyed any integer past 2^53 before the codec saw it.
//
// Nothing here traps. An AssemblyScript abort reaches the host as a RuntimeError
// carrying no path and no offset, which is exactly what a caller cannot act on,
// so every failure returns a negative length and leaves a diagnostic for
// lastError() to hand back.
//
// # Where the schema travels
//
// Out of band by default, which is the delivery the format's own README advises:
// send the section once per connection and then send ordinary messages, which
// costs nothing per message. `section()` hands back the one the last encode
// resolved; `setSchema` holds one for the decodes that follow. SELF_DESCRIBING
// puts it in front of the body instead, for a document that has to stand alone.

import { Builder } from './build'
import { D_CORRUPT, Diag, lineOf } from './diag'
import { inferPlan } from './infer'
import { inspect } from './inspect'
import { parseJSON } from './json'
import { toJSON } from './message'
import { Plan } from './plan'
import { buildSection, parseSection } from './section'
import { verify } from './verify'

/** Encode flags. */
export const SELF_DESCRIBING: i32 = 1
export const VERIFY: i32 = 2
/**
 * Offer every string to the packed encoding, keeping it where it is smaller.
 *
 * Off by default, as Go's SetPacked5 is. It cannot make a message larger — the
 * choice is per string and the raw form wins ties — but it costs a pass over
 * every character on both sides, so it is for a wire that is size-bound.
 */
export const PACK_STRINGS: i32 = 4

/** Held in globals so the collector cannot reclaim them between calls. */
let input: Uint8Array = new Uint8Array(0)
let result: Uint8Array = new Uint8Array(0)
let schema: Plan | null = null
let lastSection: Uint8Array = new Uint8Array(0)
let source: Uint8Array = new Uint8Array(0)
const diag = new Diag()

/** Reserves n bytes for the caller to write the input into. */
export function alloc(n: i32): usize {
  input = new Uint8Array(n)
  return input.dataStart
}

/** Where the last successful call left its output. */
export function resultPtr(): usize {
  return result.dataStart
}

/**
 * JSON to a colbin message.
 *
 * Returns the message length, or -1 with a diagnostic in lastError(). The
 * section for it is at `section()`; with SELF_DESCRIBING it is in front of the
 * body as well, and the root byte says so.
 *
 * VERIFY makes the encoder read its own output back and compare it against the
 * input before returning, which is what turns "a decodable message or an error,
 * never anything else" from an argument into a check (PLAN.md §4.5). It costs
 * about one decode. Leave it on unless you have measured and decided.
 */
export function encode(len: i32, flags: i32): i32 {
  diag.reset()
  source = input.subarray(0, len)
  lastSection = new Uint8Array(0)

  const doc = parseJSON(source, diag)
  if (doc == null) return fail()
  const inferred = inferPlan(doc, diag)
  if (inferred == null) return fail()

  const plan = inferred.plan
  const section = buildSection(plan)

  const builder = new Builder(doc, diag)
  builder.packStrings = (flags & PACK_STRINGS) != 0
  const selfDescribing = (flags & SELF_DESCRIBING) != 0
  if (selfDescribing) {
    // [root with the schema bit] [section] [body]. The body is byte for byte
    // what the out-of-band form writes; only the first byte and the section in
    // front of it differ.
    builder.out.writeByte(plan.isWide ? 0xdc : 0xd4)
    builder.out.writeBytes(section, 0, section.length)
    builder.run(plan, inferred.root)
  } else {
    builder.build(plan, inferred.root, false)
  }
  if (!diag.ok) return fail()
  const message = builder.out.take()

  if ((flags & VERIFY) != 0) {
    const decoded = toJSON(message, selfDescribing ? null : plan)
    if (!decoded.ok) {
      diag.fail(D_CORRUPT, -1, '', 'the encoder wrote a message it cannot read back: ' + decoded.error)
      return fail()
    }
    if (!verify(doc, inferred.root, decoded.json, diag)) return fail()
  }

  lastSection = section
  result = message
  return result.length
}

/** The schema section for the last encode, to send once per connection. */
export function section(): i32 {
  result = lastSection
  return result.length
}

/**
 * Parses a schema section from the input buffer and holds it for the decodes
 * that follow. A length of zero clears it.
 */
export function setSchema(len: i32): i32 {
  diag.reset()
  if (len == 0) {
    schema = null
    return 0
  }
  const parsed = parseSection(input.subarray(0, len))
  if (!parsed.ok) {
    diag.fail(D_CORRUPT, -1, '', 'colbin: ' + parsed.error)
    return -1
  }
  schema = parsed.plan
  return 0
}

/**
 * A colbin message to JSON text, using the held schema or — when byte 0 says so
 * — the message's own.
 */
export function decode(len: i32): i32 {
  diag.reset()
  const decoded = toJSON(input.subarray(0, len), schema)
  if (!decoded.ok) {
    diag.fail(D_CORRUPT, -1, '', decoded.error)
    return -1
  }
  result = decoded.json
  return result.length
}

/**
 * The field tree of a message, with a byte span on every node.
 *
 * The spans are absolute offsets into the message handed in, so a caller can
 * highlight them without knowing where the body starts behind the section.
 */
export function inspectMessage(len: i32): i32 {
  diag.reset()
  const tree = inspect(input.subarray(0, len), schema)
  if (!tree.ok) {
    diag.fail(D_CORRUPT, -1, '', tree.error)
    return -1
  }
  result = tree.json
  return result.length
}

/** The last diagnostic as JSON, at resultPtr. */
export function lastError(): i32 {
  diag.line = diag.offset < 0 ? -1 : lineOf(source, diag.offset)
  result = diag.encode()
  return result.length
}

function fail(): i32 {
  return -1
}
