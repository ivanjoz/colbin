// The one way the decoder fails: a thrown ColbinError, the same class the wasm
// handle throws, so a caller catches one type whichever decoder it imported.
//
// The messages are short and few: what failed is the byte at `offset`, and a
// decoder meant to weigh little does not carry a sentence per check. The Go and
// Rust readers say more about the same bytes.

import { ColbinError } from '../errors.ts'

export function fail(message: string, offset = -1): never {
  // Code 2 is the wire-level refusal, the one the module's decode uses.
  throw new ColbinError({ code: 2, offset, line: -1, path: '', message: 'colbin: ' + message, warnings: [] })
}

export const TRUNCATED = 'the message ends inside a field'
export const BAD_DESCRIPTOR = 'an unassigned or mismatched descriptor'
export const TOO_WIDE = 'a value wider than its field'
export const TOO_DEEP = 'nested deeper than 128 levels'
export const TOO_MANY_ROWS = 'more rows than this decoder allocates'
/** A schema section that is malformed, or does not describe the message. */
export const BAD_SECTION = 'a schema section that does not fit the message'
/** A table whose columns, groups, lengths or presence disagree with its rows. */
export const BAD_TABLE = 'a table whose columns disagree with its rows'
export const BAD_DICTIONARY = 'a malformed dictionary'
