// What every entry re-exports: the error, and the types. The three entries
// differ only in where they find the `.wasm`, so everything that is not that
// lives here and each of them does `export * from './shared.ts'`.

export { ColbinError } from './errors.ts'
export type { Diagnostic } from './errors.ts'

export type {
  CodecEntry,
  Convenience,
  DecodeOptions,
  Field,
  Marshaled,
  MarshalOptions,
  OpenOptions,
  Path,
  Report,
  UnmarshalOptions,
  WasmSource,
} from './core.ts'

export type { Column, ColumnTable, ReadRowsOptions } from './materialize.ts'
