# ext-parser-ts

TypeScript parser plugin for **Onfly**. Built on [gotreesitter](https://github.com/odvcencio/gotreesitter) (pure-Go tree-sitter, no CGO) with the tree-sitter-typescript and tsx grammars. Compiles to a WASI module for Onfly's wazero sandbox, or a native binary for stdio use.

```bash
onfly ext install typescript
```

Handles `.ts`, `.tsx`, `.mts` and `.cts` (`.d.ts` included). JavaScript is not covered here.

## Chunks

Input is one file on stdin, argv[1] is its path, output is the standard `[]Chunk` JSON. `.tsx` files use the TSX grammar, everything else the plain TypeScript one (the two disagree on `<T>x` casts and generic arrow functions).

| `type` | Source node | Notes |
|---|---|---|
| `import` | `import_statement`, `import x = ...` | `name` = module specifier |
| `export` | `export * from`, `export { a }`, `export default expr`, `export =` | declarations are chunked as themselves |
| `namespace` | `namespace A.B`, `module M`, `declare module "m"`, `declare global` | header only; the body is chunked with `namespace` set |
| `class` | `class_declaration`, `abstract class`, `export default class` | header only (JSDoc, decorators, signature); members are separate chunks |
| `interface` `enum` `type` | `interface_declaration`, `enum_declaration`, `type_alias_declaration` | whole declaration |
| `method` `property` `static_block` | class body members | `receiver` = owning class; arrow-function fields are `method` |
| `function` | function declarations, and `const f = () => {}` / `function` expressions | overloads and `declare function` are `function` with `bodyless=true` |
| `constant` `variable` | top-level `const` / `let` and `var` | one chunk per declarator when a statement declares several |
| `statement` | other top-level code | consecutive statements merged |
| `file` | whole file | fallback when nothing else matched |

Metadata: `name`, `fqn`, `namespace`, `receiver`, `start_line`, `end_line`, and where they apply `signature`, `params`, `return_type`, `type_params`, `type`, `kind`, `visibility`, `modifiers`, `decorators`, `extends`, `implements`, `exported`, `default`, `bodyless`, `part`.

JSDoc, `//` comments directly above a declaration, and decorators are part of the chunk. Bodies over 8 KB are split at line boundaries (`part=1/3`). Files over 80 KiB are not parsed: they come back as plain `file` chunks (`reason=too_large`), and files over 2 MiB yield `[]`. The host cancels a parse at 5 s (and then keeps the stale chunks); an 80 KiB file takes about 0.9 s on a laptop. Syntax errors do not fail the file: what parsed is chunked and a warning goes to stderr.

## Known limitations

- The wasm is about 18 MB (tree-sitter runtime plus both grammars). Expect a few seconds to compile it once per host start and about 100 ms per file, which is the grammar load.
- `export default abstract class {}` is a grammar error upstream; the file is still chunked, but that class comes back as an `export` chunk and its members are not chunked.
- Upstream grammar gaps (for example `export type * from` and `import("x").T` in a `declare const` type) give a local parse error and a stderr warning; every other declaration is still chunked.
- Declarations inside blocks (`if`, `try`) and function bodies are part of their parent chunk, not separate chunks.
- Vue / Svelte / Angular templates are not parsed; only the `.ts` files around them.

## Build

```bash
make wasm     # bin/ts-parser.wasm (reproducible, Go 1.27+)
make native   # bin/ts-parser
make test
```

## Use

```yaml
indexer:
  include: ["**/*.ts", "**/*.tsx"]
  parsers:
    - extensions: [".ts", ".tsx", ".mts", ".cts"]
      type: "wasm"
      path: ".onfly/plugins/ts-parser.wasm"
```

Third-party licenses: see THIRD_PARTY_NOTICES.
