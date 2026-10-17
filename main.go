package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

const (
	// The host cancels a parse after 5 s, and a cancelled file keeps its stale
	// chunks. A deadline inside the plugin is not an option: the host does not
	// enable wazero's real clocks, so time.Now() is fake there. Big files are
	// therefore not parsed; they become plain `file` chunks.
	maxParseSize    = 80 << 10
	maxFallbackSize = 2 << 20 // above this the file is skipped (generated/vendored)
	maxChunkSize    = 8 << 10 // longer bodies are split at line boundaries
)

// Chunk is the universal contract defined in the Onfly SDD.
type Chunk struct {
	FilePath string            `json:"file_path"`
	Type     string            `json:"type"`
	Content  string            `json:"content"`
	Metadata map[string]string `json:"metadata"`
}

// fatalError writes to stderr so the Onfly core can log it without
// breaking the stdout JSON pipeline.
func fatalError(err error) {
	fmt.Fprintf(os.Stderr, "onfly-ts-parser error: %v\n", err)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fatalError(fmt.Errorf("missing file path argument"))
	}
	filePath := os.Args[1]
	src, err := io.ReadAll(os.Stdin)
	if err != nil {
		fatalError(fmt.Errorf("failed to read stdin: %w", err))
	}
	chunks, err := parse(filePath, src)
	if err != nil {
		fatalError(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(chunks); err != nil {
		fatalError(fmt.Errorf("failed to encode json: %w", err))
	}
}

type walker struct {
	src    []byte
	lang   *gotreesitter.Language
	file   string
	lines  []int // byte offset of each line start
	chunks []Chunk
}

func parse(filePath string, src []byte) (chunks []Chunk, err error) {
	defer func() {
		if r := recover(); r != nil {
			chunks, err = nil, fmt.Errorf("parser panic: %v", r)
		}
	}()
	// Initialized empty so JSON serializes as [] instead of null.
	chunks = []Chunk{}
	if len(src) == 0 {
		return chunks, nil
	}
	w := &walker{src: src, file: filePath, lines: []int{0}, chunks: chunks}
	for i, b := range src {
		if b == '\n' {
			w.lines = append(w.lines, i+1)
		}
	}
	switch {
	case len(src) > maxFallbackSize:
		fmt.Fprintf(os.Stderr, "onfly-ts-parser: %s is %d bytes, skipped\n", filePath, len(src))
		return chunks, nil
	case len(src) > maxParseSize:
		return w.wholeFile("too_large"), nil
	}
	// JSX only parses under the TSX grammar, and `<T>x` casts only under the plain one.
	w.lang = grammars.TypescriptLanguage()
	if strings.EqualFold(path.Ext(filePath), ".tsx") {
		w.lang = grammars.TsxLanguage()
	}
	tree, err := gotreesitter.NewParser(w.lang).Parse(src)
	if err != nil {
		return nil, fmt.Errorf("failed to parse typescript file: %w", err)
	}
	root := tree.RootNode()
	if root.HasError() {
		fmt.Fprintf(os.Stderr, "onfly-ts-parser: %s has syntax errors, chunking what parsed\n", filePath)
	}
	w.block(root, "")
	if len(w.chunks) == 0 {
		// Nothing declarable (only comments, an empty module): keep the file searchable.
		return w.wholeFile(""), nil
	}
	return w.chunks, nil
}

// wholeFile returns the file as `file` chunks (split by emit at line boundaries).
func (w *walker) wholeFile(reason string) []Chunk {
	meta := map[string]string{"name": w.file}
	if reason != "" {
		meta["reason"] = reason
	}
	w.emit("file", 0, uint32(len(w.src)), meta)
	return w.chunks
}

// ---- helpers ----

func (w *walker) typ(n *gotreesitter.Node) string { return n.Type(w.lang) }
func (w *walker) text(n *gotreesitter.Node) string {
	if n == nil {
		return ""
	}
	return string(w.src[n.StartByte():n.EndByte()])
}
func (w *walker) field(n *gotreesitter.Node, name string) *gotreesitter.Node {
	return n.ChildByFieldName(name, w.lang)
}

// children returns the direct children of n whose type is in types.
func (w *walker) children(n *gotreesitter.Node, types ...string) []*gotreesitter.Node {
	var out []*gotreesitter.Node
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		t := w.typ(c)
		for _, want := range types {
			if t == want {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// docStart widens a node's start to include the comments (JSDoc or //) that
// directly precede it, with no blank line in between.
func (w *walker) docStart(n *gotreesitter.Node) uint32 {
	start := n.StartByte()
	for p := n.PrevSibling(); p != nil && w.typ(p) == "comment"; p = p.PrevSibling() {
		if strings.Count(string(w.src[p.EndByte():start]), "\n") > 1 {
			break
		}
		start = p.StartByte()
	}
	return start
}

func (w *walker) line(off uint32) int {
	return sort.Search(len(w.lines), func(i int) bool { return w.lines[i] > int(off) })
}

func (w *walker) emit(typ string, start, end uint32, meta map[string]string) {
	for end > start && strings.TrimSpace(string(w.src[end-1:end])) == "" {
		end--
	}
	if end <= start {
		return
	}
	content := string(w.src[start:end])
	if len(content) <= maxChunkSize {
		meta["start_line"] = strconv.Itoa(w.line(start))
		meta["end_line"] = strconv.Itoa(w.line(end - 1))
		w.chunks = append(w.chunks, Chunk{FilePath: w.file, Type: typ, Content: content, Metadata: meta})
		return
	}
	var parts []string
	for len(content) > maxChunkSize {
		cut := strings.LastIndex(content[:maxChunkSize], "\n") + 1 // parts start on a line boundary
		if cut <= 0 {
			cut = maxChunkSize
			for cut > 0 && !utf8.RuneStart(content[cut]) {
				cut--
			}
		}
		parts = append(parts, content[:cut])
		content = content[cut:]
	}
	parts = append(parts, content)
	off := start
	for i, p := range parts {
		m := make(map[string]string, len(meta)+3)
		for k, v := range meta {
			m[k] = v
		}
		m["start_line"] = strconv.Itoa(w.line(off))
		off += uint32(len(p))
		m["end_line"] = strconv.Itoa(w.line(off - 1))
		m["part"] = fmt.Sprintf("%d/%d", i+1, len(parts))
		w.chunks = append(w.chunks, Chunk{FilePath: w.file, Type: typ, Content: p, Metadata: m})
	}
}

func join(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "." + name
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'' || s[0] == '`') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// ---- walk ----

// decl describes one declaration after its `export` / `declare` wrappers are peeled.
// outer is the node whose full text becomes the chunk (it keeps `export default`,
// `declare` and leading decorators); n is the declaration itself.
type decl struct {
	outer, n *gotreesitter.Node
	exported bool
	deflt    bool
	declare  bool
}

// peel unwraps export_statement and ambient_declaration around a declaration.
func (w *walker) peel(c *gotreesitter.Node) decl {
	d := decl{outer: c, n: c}
	for {
		switch w.typ(d.n) {
		case "export_statement":
			d.exported = true
			for i := 0; i < d.n.ChildCount(); i++ {
				if w.typ(d.n.Child(i)) == "default" {
					d.deflt = true
				}
			}
			inner := w.field(d.n, "declaration")
			if inner == nil {
				inner = w.field(d.n, "value")
			}
			if inner == nil {
				return d
			}
			d.n = inner
		case "ambient_declaration":
			d.declare = true
			// `declare const x`, `declare function f()`: the declaration is the named child.
			var inner *gotreesitter.Node
			for i := 0; i < d.n.ChildCount(); i++ {
				if ch := d.n.Child(i); ch.IsNamed() && w.typ(ch) != "comment" {
					inner = ch
					break
				}
			}
			if inner == nil {
				return d
			}
			d.n = inner
		case "expression_statement":
			// `namespace A.B {}` parses as a statement wrapping internal_module.
			if d.n.NamedChildCount() == 1 && w.typ(d.n.NamedChild(0)) == "internal_module" {
				d.n = d.n.NamedChild(0)
				continue
			}
			return d
		default:
			return d
		}
	}
}

// block walks the statements of the program or a namespace body.
func (w *walker) block(parent *gotreesitter.Node, ns string) {
	var run []*gotreesitter.Node
	flush := func() {
		if len(run) == 0 {
			return
		}
		first, last := run[0], run[len(run)-1]
		w.emit("statement", w.docStart(first), last.EndByte(), map[string]string{"name": "statements", "namespace": ns})
		run = nil
	}
	for i := 0; i < parent.ChildCount(); i++ {
		c := parent.Child(i)
		t := w.typ(c)
		switch t {
		case "comment", "hash_bang_line", "{", "}":
			continue
		case "import_statement":
			flush()
			w.importStmt(c, ns)
			continue
		}
		d := w.peel(c)
		if w.declaration(d, ns) {
			flush()
			continue
		}
		if t == "export_statement" {
			flush()
			w.exportStmt(d, ns)
			continue
		}
		run = append(run, c)
	}
	flush()
}

// declaration emits the chunks for d and reports whether it recognised it.
func (w *walker) declaration(d decl, ns string) bool {
	m := map[string]string{"namespace": ns}
	if d.exported {
		m["exported"] = "true"
	}
	if d.deflt {
		m["default"] = "true"
	}
	switch t := w.typ(d.n); t {
	case "function_declaration", "generator_function_declaration", "function_signature":
		w.function(d, ns, m)
	case "function_expression", "arrow_function", "generator_function": // export default () => {}
		if !d.deflt {
			return false
		}
		w.function(d, ns, m)
	case "class_declaration", "abstract_class_declaration", "class":
		w.class(d, ns, m)
	case "interface_declaration", "enum_declaration", "type_alias_declaration":
		kind := map[string]string{"interface_declaration": "interface", "enum_declaration": "enum", "type_alias_declaration": "type"}[t]
		name := w.text(w.field(d.n, "name"))
		m["name"], m["fqn"] = name, join(ns, name)
		w.typeParams(d.n, m)
		if t == "interface_declaration" {
			for _, c := range w.children(d.n, "extends_type_clause") {
				m["extends"] = strings.TrimSpace(strings.TrimPrefix(w.text(c), "extends"))
			}
		}
		w.modifiers(d.n, m, d.declare)
		w.emit(kind, w.docStart(d.outer), d.outer.EndByte(), m)
	case "internal_module", "module":
		name := unquote(w.text(w.field(d.n, "name")))
		m["name"], m["fqn"] = name, join(ns, name)
		w.modifiers(d.n, m, d.declare)
		body := w.field(d.n, "body")
		end := d.outer.EndByte()
		if body != nil {
			end = body.StartByte()
		}
		w.emit("namespace", w.docStart(d.outer), end, m)
		if body != nil {
			w.block(body, join(ns, name))
		}
	case "lexical_declaration", "variable_declaration":
		w.variables(d, ns, m)
	case "import_alias":
		m["name"] = w.text(d.n.NamedChild(0))
		w.emit("import", d.outer.StartByte(), d.outer.EndByte(), m)
	default:
		if d.declare && w.typ(d.n) == "statement_block" {
			// `declare global { ... }`: members belong to the global scope.
			m["name"], m["fqn"] = "global", "global"
			w.emit("namespace", w.docStart(d.outer), d.n.StartByte(), m)
			w.block(d.n, "global")
			return true
		}
		return false
	}
	return true
}

func (w *walker) importStmt(n *gotreesitter.Node, ns string) {
	name := unquote(w.text(w.field(n, "source")))
	if name == "" { // `import x = require("y")`
		name = strings.Join(strings.Fields(strings.TrimSuffix(strings.TrimPrefix(w.text(n), "import"), ";")), " ")
	}
	w.emit("import", n.StartByte(), n.EndByte(), map[string]string{"name": name, "namespace": ns})
}

// exportStmt handles exports that declare nothing: `export * from`, `export { a }`,
// `export = x` and `export default expr`.
func (w *walker) exportStmt(d decl, ns string) {
	m := map[string]string{"name": "exports", "namespace": ns, "exported": "true"}
	if src := w.field(d.outer, "source"); src != nil {
		m["name"] = unquote(w.text(src))
	}
	if d.deflt {
		m["name"], m["default"] = "default", "true"
	}
	w.emit("export", w.docStart(d.outer), d.outer.EndByte(), m)
}

func (w *walker) typeParams(n *gotreesitter.Node, m map[string]string) {
	if p := w.field(n, "type_parameters"); p != nil {
		m["type_params"] = w.text(p)
	}
}

func (w *walker) decorators(n *gotreesitter.Node, m map[string]string) {
	for _, c := range w.children(n, "decorator") {
		if m["decorators"] != "" {
			m["decorators"] += " "
		}
		m["decorators"] += w.text(c)
	}
}

// modifiers collects the keyword tokens a declaration carries and the visibility.
func (w *walker) modifiers(n *gotreesitter.Node, m map[string]string, declare bool) {
	var mods []string
	if declare {
		mods = append(mods, "declare")
	}
	for i := 0; i < n.ChildCount(); i++ {
		c := n.Child(i)
		switch t := w.typ(c); t {
		case "accessibility_modifier":
			m["visibility"] = w.text(c)
		case "override_modifier":
			mods = append(mods, "override")
		case "static", "abstract", "readonly", "async", "get", "set", "const", "declare", "accessor", "*":
			if t == "*" {
				t = "generator"
			}
			mods = append(mods, t)
		}
		if w.typ(c) == "class_body" || w.typ(c) == "statement_block" || w.typ(c) == "enum_body" {
			break
		}
	}
	if len(mods) > 0 {
		m["modifiers"] = strings.Join(mods, " ")
	}
}

// callable fills signature, params and return type for functions, methods and arrow functions.
// head is the node the signature starts at, fn the node carrying the fields.
func (w *walker) callable(head, fn *gotreesitter.Node, m map[string]string) {
	end := fn.EndByte()
	if body := w.field(fn, "body"); body != nil {
		end = body.StartByte()
	}
	if end > head.StartByte() {
		m["signature"] = strings.Join(strings.Fields(string(w.src[head.StartByte():end])), " ")
	}
	if p := w.field(fn, "parameters"); p != nil {
		m["params"] = w.text(p)
	} else if p := w.field(fn, "parameter"); p != nil { // `x => x`
		m["params"] = w.text(p)
	}
	if r := w.field(fn, "return_type"); r != nil {
		m["return_type"] = strings.TrimSpace(strings.TrimPrefix(w.text(r), ":"))
	}
	w.typeParams(fn, m)
}

func isFunc(t string) bool {
	return t == "arrow_function" || t == "function_expression" || t == "function" || t == "generator_function"
}

func (w *walker) function(d decl, ns string, m map[string]string) {
	name := w.text(w.field(d.n, "name"))
	if name == "" {
		name = "default"
	}
	m["name"], m["fqn"] = name, join(ns, name)
	w.callable(d.outer, d.n, m)
	w.modifiers(d.n, m, d.declare)
	if w.typ(d.n) == "function_signature" {
		m["bodyless"] = "true" // overload, abstract or ambient declaration
	}
	w.emit("function", w.docStart(d.outer), d.outer.EndByte(), m)
}

func (w *walker) variables(d decl, ns string, m map[string]string) {
	kind := "var"
	for i := 0; i < d.n.ChildCount(); i++ {
		if t := w.typ(d.n.Child(i)); t == "const" || t == "let" || t == "var" || t == "using" {
			kind = t
			break
		}
	}
	vars := w.children(d.n, "variable_declarator")
	for _, v := range vars {
		vm := make(map[string]string, len(m)+4)
		for k, x := range m {
			vm[k] = x
		}
		name := strings.Join(strings.Fields(w.text(w.field(v, "name"))), " ")
		vm["name"], vm["fqn"] = name, join(ns, name)
		vm["kind"] = kind
		if tn := w.field(v, "type"); tn != nil {
			vm["type"] = strings.TrimSpace(strings.TrimPrefix(w.text(tn), ":"))
		}
		if d.declare {
			vm["modifiers"] = "declare"
		}
		// One declarator: the chunk is the whole statement. Several: each gets its own text.
		start, end := w.docStart(d.outer), d.outer.EndByte()
		if len(vars) > 1 {
			start, end = v.StartByte(), v.EndByte()
		}
		typ := "variable"
		if kind == "const" {
			typ = "constant"
		}
		if val := w.field(v, "value"); val != nil && isFunc(w.typ(val)) {
			typ = "function"
			head := v
			if len(vars) == 1 {
				head = d.outer // keep `export const` in the signature
			}
			w.callable(head, val, vm)
			w.modifiers(val, vm, d.declare)
		}
		w.emit(typ, start, end, vm)
	}
}

func (w *walker) class(d decl, ns string, m map[string]string) {
	name := w.text(w.field(d.n, "name"))
	if name == "" {
		name = "default"
	}
	fqn := join(ns, name)
	m["name"], m["fqn"] = name, fqn
	w.typeParams(d.n, m)
	w.decorators(d.n, m)
	if w.typ(d.outer) == "export_statement" {
		w.decorators(d.outer, m)
	}
	w.modifiers(d.n, m, d.declare)
	for _, h := range w.children(d.n, "class_heritage") {
		for _, c := range w.children(h, "extends_clause") {
			m["extends"] = strings.TrimSpace(strings.TrimPrefix(w.text(c), "extends"))
		}
		for _, c := range w.children(h, "implements_clause") {
			m["implements"] = strings.TrimSpace(strings.TrimPrefix(w.text(c), "implements"))
		}
	}
	body := w.field(d.n, "body")
	end := d.outer.EndByte()
	if body != nil {
		end = body.StartByte()
	}
	// Header only: members are separate chunks that point back through `receiver`.
	w.emit("class", w.docStart(d.outer), end, m)
	if body == nil {
		return
	}
	for i := 0; i < body.ChildCount(); i++ {
		w.member(body.Child(i), ns, fqn)
	}
}

func (w *walker) member(c *gotreesitter.Node, ns, class string) {
	m := map[string]string{"receiver": class, "namespace": ns}
	t := w.typ(c)
	switch t {
	case "method_definition", "abstract_method_signature", "method_signature":
		name := w.methodName(c)
		m["name"], m["fqn"] = name, class+"."+name
		w.callable(c, c, m)
		w.decorators(c, m)
		w.modifiers(c, m, false)
		if w.text(w.field(c, "name")) == "async" && name != "async" {
			m["modifiers"] = strings.TrimSpace(m["modifiers"] + " async")
		}
		w.privateName(name, m)
		if t != "method_definition" || w.field(c, "body") == nil {
			m["bodyless"] = "true"
		}
		w.emit("method", w.docStart(c), c.EndByte(), m)
	case "public_field_definition", "property_signature":
		name := w.text(w.field(c, "name"))
		m["name"], m["fqn"] = name, class+"."+name
		if tn := w.field(c, "type"); tn != nil {
			m["type"] = strings.TrimSpace(strings.TrimPrefix(w.text(tn), ":"))
		}
		w.decorators(c, m)
		w.modifiers(c, m, false)
		w.privateName(name, m)
		typ := "property"
		if val := w.field(c, "value"); val != nil && isFunc(w.typ(val)) {
			typ = "method" // `handle = () => {}`: a bound method
			w.callable(c, val, m)
		}
		w.emit(typ, w.docStart(c), c.EndByte(), m)
	case "index_signature":
		m["name"] = "[index]"
		m["fqn"] = class + ".[index]"
		w.emit("property", w.docStart(c), c.EndByte(), m)
	case "class_static_block":
		m["name"] = "static"
		m["fqn"] = class + ".static"
		w.emit("static_block", w.docStart(c), c.EndByte(), m)
	}
}

func (w *walker) privateName(name string, m map[string]string) {
	if strings.HasPrefix(name, "#") {
		m["visibility"] = "private"
	}
}

// methodName works around a grammar quirk: in `static async *name() {}` the
// `name` field points at the `async` keyword and the real name is a later child.
func (w *walker) methodName(c *gotreesitter.Node) string {
	name := w.text(w.field(c, "name"))
	if name != "async" && name != "static" && name != "get" && name != "set" {
		return name
	}
	for i := 0; i < c.ChildCount(); i++ {
		ch := c.Child(i)
		if t := w.typ(ch); t == "property_identifier" && w.text(ch) != name {
			return w.text(ch)
		}
	}
	return name
}
