// A whole message, from its first byte to JSON text.
//
// Port of codec/json.go's entry points and codec/root.go's reservation. It is
// the layer above walk.ts: resolve where the body starts and which key width it
// uses, then hand the body to the walk.

import { Plan } from './plan'
import { JSONSink } from './jsontext'
import { Section, parseSection } from './section'
import { Walker } from './walk'

/**
 * Every first byte colbin writes or accepts.
 *
 * A message is one value: a descriptor, then its payload. The root descriptor is
 * an ordinary K8 descriptor — `1 ccc dddd` — and its class is STRUCT, class 5,
 * so `1 101 dddd` = 0xD0 | detail. That is the whole reason a colbin message
 * starts with 0xD-something; it was not chosen as a magic number, it is what the
 * descriptor rules produce.
 *
 * The other 240 first bytes are guaranteed never to be written by colbin and
 * belong to the application, so this is a range check rather than a validation.
 */
export const ROOT_FIRST: u8 = 0xd0
export const ROOT_LAST: u8 = 0xdf

/** Root detail bits. Four of sixteen are assigned; the rest must be refused,
 * which is what leaves room for the format to grow. */
const ROOT_WIDE: u8 = 0x08
const ROOT_SCHEMA: u8 = 0x04

export class Decoded {
  json: Uint8Array = new Uint8Array(0)
  error: string = ''

  @inline get ok(): bool {
    return this.error.length == 0
  }
}

/**
 * A message as JSON text.
 *
 * `schema` may be null when the message carries its own section — byte 0 with
 * the schema bit — which is the self-describing delivery. Passing one that the
 * message does not need is the out-of-band delivery, and the one a stream should
 * use: the section is sent once per connection and costs nothing per message.
 */
export function toJSON(data: Uint8Array, schema: Plan | null): Decoded {
  const out = new Decoded()
  if (data.length == 0) {
    out.error = 'colbin: empty message'
    return out
  }
  const root = unchecked(data[0])
  if (root < ROOT_FIRST || root > ROOT_LAST) {
    out.error = 'colbin: byte 0 is ' + hexByte(root) + ', which is not a colbin root'
    return out
  }
  if ((root & ~(ROOT_WIDE | ROOT_SCHEMA)) != ROOT_FIRST) {
    out.error = 'colbin: root byte ' + hexByte(root) + ' is not assigned by this version'
    return out
  }

  let plan = schema
  let at = 1
  if ((root & ROOT_SCHEMA) != 0) {
    const section: Section = parseSection(data.subarray(1))
    if (!section.ok) {
      out.error = 'colbin: ' + section.error
      return out
    }
    at = 1 + section.size
    // A caller's schema wins over the message's own only if it passed one; the
    // section is parsed either way, because the body starts behind it.
    if (plan == null) plan = section.plan
  } else if (plan == null) {
    out.error =
      'colbin: byte 0 is ' +
      hexByte(root) +
      ', which carries no schema section: pass the schema, or encode self-describing'
    return out
  }

  const sink = new JSONSink()
  const walker = new Walker(sink)
  walker.root(plan, data.subarray(at), (root & ROOT_WIDE) != 0)
  if (!walker.ok) {
    out.error = walker.error.length > 0 ? walker.error : sink.error
    return out
  }
  out.json = sink.take()
  return out
}

function hexByte(value: u8): string {
  const digits = '0123456789abcdef'
  return '0x' + digits.charAt(<i32>(value >> 4)) + digits.charAt(<i32>(value & 0xf))
}
