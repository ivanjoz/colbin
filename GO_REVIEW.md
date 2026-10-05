# Revisión de la implementación Go de colbin

*2026-10-04, sobre `0e9377a` (0.3.1).*

Revisé las ~28 000 líneas de Go: el paquete raíz, `wire`, `codec`, `column`, `packed5`, `corpus`, `experiments` y los generadores de vectores. `go vet`, `gofmt` y `go test ./...` (también con `-race`) pasan limpios. Cada bug de abajo se reprodujo con un programa fuera del repo; los marcados con ✔ los reproduje en persona. Lo que solo es sospecha está dicho como tal.

**Veredicto:** la codificación de valores es cuidadosa y rápida, y en los casos normales los datos vuelven exactos. Los problemas son tres:

1. **El decodificador no aguanta entrada maliciosa.** Mensajes de 2 a 15 bytes cuelgan o matan el proceso.
2. **El mismo `switch` por campo está copiado unas 14 veces** (narrow/wide × plano/anidado × puntero/generado). Las copias ya divergen, y una de esas diferencias corrompe datos sin dar error.
3. **Comentarios y README describen versiones anteriores del formato.**

Lo que sí está sólido:

- `column`: 13 M ejecuciones de fuzz sin fallos.
- El núcleo de `packed5`.
- Los vectores, sincronizados y deterministas incluso en 386.
- El check de cero dependencias en CI.
- La escritura de números en JSON coincide con `encoding/json` (−0, 1e21, uint64 > 2⁶³).

## 1. Bugs

### Corrupción silenciosa ✔

`codec/narrow_composite.go:77-92` y `wire/narrow_composite.go:77-83`. Se da cuando un struct con claves de 4 bits contiene un `[]Row` de 8 o más filas (a partir de ahí se escribe como tabla) y `Row` usa claves de 8 bits. El escritor narrow hace `key<<4` y trunca las claves de columna:

```go
type R2 struct { X int64 `cb:"2"`; Y int64 `cb:"18"` }
type B2 struct { Rows []R2 `cb:"1"` }
// entra {X:100 Y:900}  →  sale {X:900 Y:0}, err == nil
```

Si `Row` no tiene tags (claves por hash), el resultado es un error en vez de datos corruptos: con 3 filas funciona y con 20 `Unmarshal` falla. El bit `k8` que documenta `wire/narrow_composite.go:11-14` nunca se escribe ni se lee.

### Entrada maliciosa

| Entrada | Efecto | Dónde |
|---|---|---|
| `d0 01` (2 B) sobre `struct{A any}` | `Unmarshal` en bucle infinito ✔ | `codec/codec.go:737-795`: el switch no tiene `case` para `opAny`/`opAnys` ni `default` |
| `d8 00 30` (3 B) sobre `struct{Vals []any}` | panic `slice bounds out of range [-1:]` ✔ | ruta dinámica |
| 9 B sobre un campo `[]struct` | intenta reservar **103 GB**: `fatal error`, que no se puede recuperar ✔ | los conteos no se validan: `wire/composite.go:221-262`, `codec/composite.go:157`, `codec/maps.go:196` |
| 8 B → 224 MB · 184 B → 480 MB · 1,6 KB → 13 GB | `maxTableRows` limita filas por tabla, no filas×columnas por mensaje | `codec/table.go:226-235`, `codec/json.go:745-855` |
| ~5 MB de bytes `0xF3` | stack overflow fatal (`Skip` es recursivo) | `wire/dynamic.go:401-433`, `wire/wide.go:824-856` |
| 15 B autodescriptivos (un schema que se referencia a sí mismo dos veces por valor) | `ToJSON` cuelga (trabajo de 2¹²⁸), `DecodeAny` se queda sin memoria | `codec/json.go:379-428` |
| `*Node` anidado 25 MB, o codificar `n.Next = n` | stack overflow fatal | la ruta tipada no tiene límite de profundidad |

La solución es común a todos:

- Validar que cada conteo no supera los bytes restantes (cada elemento ocupa al menos 1 byte).
- Un presupuesto por mensaje, no por tabla.
- Un límite de profundidad en la ruta tipada.
- Un bucle en vez de recursión en `Skip`.
- Que `ParseSchema` rechace ciclos de structs por valor.
- Un `default: Fail` en cada switch.

Un fuzz de `Unmarshal` encontró el agotamiento de memoria en 3 segundos y el bucle en menos de un minuto.

### Otros bugs confirmados

- **Las lecturas de packed5 no son coherentes.** Con `SetPacked5(true)` solo empaquetan la ruta plana y la wide. En cambio `readField`, `readScalar` y el código generado usan `r.String()`, que rechaza strings empaquetados. Consecuencias:
  - El `UnmarshalColbin` generado no puede leer la salida de `Marshal` del mismo tipo.
  - El cuerpo de `MarshalSelfDescribing` ya no es idéntico al de `Marshal`, aunque `colbin.go:147` lo promete.
- **`int`/`uint`/`[]int` en 32 bits** (`codec/codec.go:363,371,415,421`) se tratan como 64 bits vía `unsafe`, así que leen y escriben 8 bytes sobre campos de 4. En 386, `{N:5, Next:7}` escribe N como `0x0700000005`. CI no prueba 386.
- **`Marshal` cuelga** con `m["a"]=m; m["b"]=m` (`codec/dynamic.go:343-403`): el error se registra, pero el recorrido sigue por los hermanos.
- **packed5 en narrow puede agrandar el mensaje** (`wire/packed.go:150-166`): un payload de más de 255 B paga una cabecera de 5 bytes, contra 2 en crudo. Contradice la promesa del README de que activarlo nunca hace un mensaje más grande.
- **`wire.Reader8`:**
  - `Strings` ignora la longitud declarada y se come el campo siguiente (`wide.go:705-765`).
  - `String`/`Bytes` devuelven bytes empaquetados como texto, sin error (`wide.go:601-615`).
  - `More` acepta un byte suelto al final (`wide.go:487`).
- **`ToJSON`:**
  - Con un map narrow de valores `any` emite `{"M":{"a":}}`, JSON inválido, con `err == nil` (`codec/json.go:905-949`).
  - Si recibe un schema y el mensaje trae el suyo, usa el del llamador y devuelve campos equivocados (`json.go:211-223`).
- **`Generate`** emite código que no compila para `int`, `uint`, `[]int` y tipos con nombre como `type Status int32` (`generate.go:38-60`).
- **Errores en casos límite:**
  - Un struct embebido no exportado se descarta sin error.
  - `Marshal(nil)` y `NewCodec[any]()` hacen panic.

## 2. Diseño

1. **El `switch` copiado unas 14 veces.** Tres de los bugs anteriores son divergencias entre copias: el bucle infinito, packed5 y la tabla narrow. El JSON inválido viene del mismo patrón en los walkers de `json.go`. La justificación de los comentarios ("el compilador no pliega un ancho que no ve") vale para el hot path, pero no obliga a copiar a mano. Se puede usar genéricos sobre el tipo de writer, o generar las copias desde una tabla, ya que el repo tiene generador. Como mínimo, un test que pase cada op por cada ruta y compare los bytes.
2. **No hay modelo de amenaza, aunque el producto se vende para navegadores y peers.** El comentario de `ParseSchema` dice "everything in it is checked", y no es cierto. Conviene fijar un invariante: `Unmarshal` nunca reserva más de O(len(data)) ni recurre más de N niveles. Así se puede testear.
3. **La ruta rápida (con tags, claves de 4 bits) es la que no admite evolución.** No puede saltar un campo desconocido, y no hay versión ni en el mensaje ni en la sección de schema; el README admite que mezclar 0.2 y 0.3 "parece datos corruptos". Mientras estén en 0.x, conviene añadir al menos un byte de versión en la sección de schema.
4. **`SetPacked5` global.** packed5 ya no decide el ancho de clave (`codec/wide.go:53-60`), así que los `Clear()` de caché de `packed5flag.go:19-26` no sirven para nada y el aviso de concurrencia está desfasado. El alcance sigue siendo incorrecto: dos librerías en el mismo proceso se pisan el ajuste. Debería ser opción por `Codec` o por tag. Sobre si debe existir siquiera: hoy da entre −11 y −18 % de bytes a cambio de 2× en codificar y 2,5–4× en decodificar, y vive en tres implementaciones. La sección del README que habla de "6× más lento" corresponde al diseño viejo.
5. **`codec` duplica la API pública.** `colbin.go` son 17 wrappers de una línea, y `codec` solo lo importa `colbin.go`. Hay dos rutas de import con documentación que ya divergió; por ejemplo, el ejemplo de `Schema`, `send(colbin.Marshal(&sale))`, no compila. Propuesta: mover `codec`, `column` y `packed5` a `internal/` y exponer `Generate` desde la raíz o desde un `cmd/`.
6. **Los maps tipados no se ordenan y los dinámicos sí** (`codec/maps.go:142` frente a `:157`). Un mismo valor da bytes distintos en cada ejecución. Eso obliga a parches: Events se excluye del fingerprint y los generadores limitan los maps a una sola entrada.
7. **API de `Codec`:**
   - `Append` escribe `null` en silencio cuando un `any` no se puede codificar; la variante que devuelve el error, `AppendChecked`, es opcional. Está al revés.
   - `FieldIDs` devuelve claves (base 0), no ids; debería llamarse `FieldKeys`.
   - `Codec[T]` rechaza raíces slice o map que `Marshal` sí acepta.
   - `Append` y `AppendJSON` devuelven `nil` en caso de error y pierden el buffer del llamador.
   - `Append` con tablas reserva 24 KB por llamada, aunque la doc dice "allocates nothing".
8. **Que `ToJSON` produzca lo mismo que `encoding/json` es falso** en registros normales:
   - Los campos ausentes van al final.
   - Los maps tipados salen en orden aleatorio.
   - Se ignoran los tags `json` y el embedding.
   - `\b` sale como `\u0008`.
   - Un `time.Time` dentro de `any` sale como `{}`, con pérdida silenciosa.

## 3. Qué sacar

| Qué | Por qué |
|---|---|
| `experiments/` (~4,6 k líneas) | Tras el rename de 8b2bd7d, el baseline "bit-varint" pasó a ser el mismo codec que se proponía, así que las etiquetas son falsas y los resultados no se pueden reproducir. stringpack se declara obsoleto en su propio README y conserva restos de sesiones con IA ("another agent owns", un prompt citado). No compila en 386. Propuesta: pasar las 3 conclusiones que faltan (p6, la tabla de clases, los tripletes uint16) a RATIONALE, marcar un tag y borrarlo. |
| `wire/bitmap.go` | Ningún encoder ni decoder produce o reconoce un bitmap. Solo lo usa un benchmark de una pregunta ya cerrada. |
| La capa de frame de packed5 (`Append`, `Decode`, `Size`, uvarint…) | No tiene consumidor en Go, Rust ni JS. Usa una longitud LEB128, lo que contradice "no size is a varint". Bastan `AppendPayload` y `AppendString`. |
| Exports de `wire` sin uso | `Reader8.Struct` (que además ignora el bit k8), `ElementStruct`, `Column8/16/32`, `WriteInts`/`ReadInts`, `MoreElements`, `Reset` |
| Código muerto | `typePlan.hasStrings` (nunca se lee); `Schema.plan` (siempre es `plans[0]`); `maximum` en `column/array.go:118`; `widthOfType` (la codificación no depende de `T`); el `case` inalcanzable de `pointer.go:284`; el `const _ = uint(15-9)` del código generado, que nunca puede fallar |
| 5 de 7 niveles de `js/vectors` (~111 KB) | No tienen consumidor desde que se borró AssemblyScript. Además, `js/vectors` y `rust/vectors` duplican unas 210 líneas, con funciones del mismo nombre y algoritmo distinto (`scatter`/`spread`). Basta un generador en `internal/`. |
| `codec.test` (5,8 MB) en la raíz | Binario de pruebas sobrante, ignorado por git |

Sobre `corpus`: es un paquete público que promete "la misma semilla da las mismas filas", y además está fijado por SHA en el repo de benchmarks, así que cualquier ajuste rompe esa promesa. Propuesta: moverlo a `internal/`. Además solo ejercita claves de 4 bits, así que los tamaños del README nunca miden la ruta wide.

## 4. Forma

Los comentarios son el mayor problema de forma. Ocupan entre el 40 y el 76 % de algunos archivos (`colbin.go` 76 %, `packed5.go` 51 %). Gran parte narra historia ("used to", "the old format", "measured X ns against Y"), y las cifras de benchmark en comentarios ya no coinciden con el README. Esa historia va en RATIONALE o en los commits; en el código basta una línea con la restricción.

Muchos son directamente falsos:

- **`codec/codec.go:14-24`:** habla de "compact-mode bridge" y "third mode", y dice que un tipo sin campos numerados "is refused". Es falso: sin tags se usa fnv8.
- **`codec/codec.go:838-850`:** la doc de `Codec` dice ser "the same idea as Codec[T]", es decir, de sí misma, y su ejemplo `buf, _ = chargeCodec.Append(...)` no compila.
- **`FieldIDs`** (`codec.go:796`): dice "in declaration order" pero devuelve un map.
- **`wide.go:24-25`:** dice que packed5 fuerza claves wide, y `wide.go:53-60` dice lo contrario. El mismo error está en `doc.go:30`, `schema.go:60` y README:139,150,199,219.
- **`go doc ./wire`** imprime "Package narrow implements … at most sixteen primitive fields" y cita `colbin.MarshalMinimal`, que no existe.
- **`column/pack.go:1`** dice "Package varint". Los errores "varint array truncated" llegan al usuario, y el ejemplo del README de `column` no compila.
- **En `wire/wide.go`**, la línea 44 dice "There is no varint anywhere", pero el varint está definido en las líneas 89-118 del mismo archivo.
- **Todos los errores de `wire`** llevan el prefijo `narrow:`, incluidos los que solo existen en la ruta wide.
- **Referencias a cosas inexistentes:** `compact.ErrSkipComposite`, `wire.go`, `column.go`, `structNarrowKeys`.
- **Restos de un buscar-y-reemplazar:** "the format field id" (`codec.go:330`), "colbin's the format" (`generate.go:142`).
- **Doc de paquete duplicada:** `colbin.go` y `doc.go` tienen ambos `// Package colbin`, así que `go doc .` imprime la introducción y la sección "# Layers" dos veces.

README desactualizado:

- Dice que los punteros a struct no están soportados (226, 245), pero sí lo están desde 2cc4c29.
- "Interfaces have no form on the wire" (575), pero `any` se transmite.
- "id above fifteen" (199) contradice los ids que empiezan en 1.
- "That is the whole module" (388) es falso mientras exista `experiments/`.

## 5. Tests y CI

- **No hay fuzz para `Unmarshal` ni para los lectores de `wire`**, que son justo la ruta que escribe con `unsafe`.
- **Tests que no verifican nada:**
  - Los `*TruncationIsRefused` descartan el error.
  - Los `…AreRefused` de `wire` hacen `_ = reader.Err()`.
  - `TestArrayNarrowerTypeNeverLarger` no puede fallar.
  - `TestConcurrentUse` prueba un pool que ya no existe.
- **Comparaciones numéricas a través de float64:** `js/vectors/encoded_test.go:91-113` da por iguales `7295013456321098765` y `…770`. Es el único check del sentido wasm→Go, y no detecta int64 rotos. `sameJSON` en `json_test.go` tiene el mismo problema.
- **El test de código generado es débil.** `TestGeneratedCodecIsUpToDate` solo comprueba que cada línea aparezca en algún sitio, y el test del generador no compila su salida; por eso nadie vio que `Generate` emite código que no compila.
- **Tests no deterministas en packed5:** iteran un map y comparten el RNG, así que los peores casos que cita el README no se pueden reproducir.
- **El baseline "a mano" de `bench_test.go:32-45`** escribe otra codificación, más corta y sin byte raíz. La fila "hand-written vs generated" del README compara salidas distintas.
- **CI:**
  - Le faltan `-race`, `GOARCH=386`, staticcheck y un fuzz corto. En 386 ni compilan los tests de `wire`, y staticcheck marca dos expresiones idénticas en tests (`packed5/decode_test.go:60`, `wire/narrow_test.go:286`).
  - `concurrency: group: pages` lo comparten todos los push y PR, lo que serializa todo el CI. Según la documentación de GitHub puede cancelar runs pendientes de PRs; no está observado.

## Prioridad sugerida

1. La corrupción silenciosa de las tablas narrow.
2. Robustez ante entrada maliciosa (conteos, presupuesto, profundidad, `default: Fail`), con fuzz de `Unmarshal` y `wire` en CI.
3. packed5 coherente en todos los lectores, y `int` en 32 bits con un job 386.
4. Unificar los switches duplicados, para que 1–3 no vuelvan a pasar.
5. Limpieza: borrar lo de la tabla de la sección 3 y reescribir los comentarios y el README desactualizados.
