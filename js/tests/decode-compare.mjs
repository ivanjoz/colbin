// Comparing what the pure-JS decoder returns with the JSON Go wrote for the
// same message.
//
// Byte-for-byte JSON is the wrong comparison here: Go writes a record's absent
// fields after its present ones, and the decoder builds every object in schema
// order. So both sides are put in one canonical text — keys sorted, numbers as
// their digits — and that is compared. Numbers are not parsed on the Go side:
// its literal is kept, so a value that differs in its last digit, or an integer
// past 2^53 that rounded, is a difference.

/** Canonical text of a decoded value. */
export function canonical(value) {
  if (value === null || value === undefined) return 'null'
  switch (typeof value) {
    case 'boolean':
      return value ? 'true' : 'false'
    case 'bigint':
      return value.toString()
    case 'number':
      if (Object.is(value, -0)) return '-0'
      if (!Number.isFinite(value)) return `!${value}`
      return String(value)
    case 'string':
      return JSON.stringify(value)
  }
  if (value instanceof Uint8Array) return JSON.stringify(Buffer.from(value).toString('base64'))
  if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`
  const keys = Object.keys(value).sort()
  return `{${keys.map((k) => `${JSON.stringify(k)}:${canonical(value[k])}`).join(',')}}`
}

/** Canonical text of Go's JSON, its numbers kept as written. */
export function canonicalJSON(text) {
  let at = 0
  const ws = () => {
    while (at < text.length && ' \t\n\r'.includes(text[at])) at++
  }
  const value = () => {
    ws()
    const c = text[at]
    if (c === '{') {
      at++
      const entries = []
      ws()
      if (text[at] === '}') {
        at++
        return '{}'
      }
      for (;;) {
        ws()
        const key = string()
        ws()
        at++ // :
        entries.push([key, value()])
        ws()
        if (text[at++] === '}') break
      }
      entries.sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0))
      return `{${entries.map(([k, v]) => `${JSON.stringify(k)}:${v}`).join(',')}}`
    }
    if (c === '[') {
      at++
      const items = []
      ws()
      if (text[at] === ']') {
        at++
        return '[]'
      }
      for (;;) {
        items.push(value())
        ws()
        if (text[at++] === ']') break
      }
      return `[${items.join(',')}]`
    }
    if (c === '"') return JSON.stringify(string())
    if (text.startsWith('true', at)) {
      at += 4
      return 'true'
    }
    if (text.startsWith('false', at)) {
      at += 5
      return 'false'
    }
    if (text.startsWith('null', at)) {
      at += 4
      return 'null'
    }
    const start = at
    while (at < text.length && '+-0123456789.eE'.includes(text[at])) at++
    return text.slice(start, at)
  }
  const string = () => {
    const start = at
    at++
    while (text[at] !== '"') at += text[at] === '\\' ? 2 : 1
    at++
    return JSON.parse(text.slice(start, at))
  }
  return value()
}
