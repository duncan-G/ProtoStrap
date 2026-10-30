// Package yaml parses the part of YAML that protostrap.yaml uses, with the standard library
// only: block mappings and sequences, flow mappings and sequences, plain and quoted
// scalars, literal and folded block scalars, and comments. Anchors, aliases, tags, merge
// keys, complex keys and multiple documents are refused rather than half supported.
//
// Scalars are kept as text with their style and position, and typed by whoever decodes
// them (see Decode), so `yes` stays a string and an error can name the line it came from.
package yaml

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Kind is a node's kind.
type Kind int

const (
	ScalarNode Kind = iota
	MapNode
	SeqNode
)

// Style is how a scalar was written.
type Style int

const (
	Plain Style = iota
	SingleQuoted
	DoubleQuoted
	Literal // A literal block scalar starts with |.
	Folded  // A folded block scalar starts with >.
)

// Node is one value in the document, with where it starts (1-based).
type Node struct {
	Kind   Kind
	Style  Style
	Value  string  // It holds a scalar's text.
	Pairs  []Pair  // They hold a mapping's entries, in document order.
	Items  []*Node // They hold a sequence's items.
	Line   int
	Column int
}

// Pair is one entry of a mapping.
type Pair struct {
	Key   *Node
	Value *Node
}

// IsNull reports whether the node is YAML's null: an empty value, ~ or null.
func (n *Node) IsNull() bool {
	if n == nil {
		return true
	}
	if n.Kind != ScalarNode || n.Style != Plain {
		return false
	}
	switch n.Value {
	case "", "~", "null", "Null", "NULL":
		return true
	}
	return false
}

// Get returns a mapping's value for key, or nil.
func (n *Node) Get(key string) *Node {
	if n == nil || n.Kind != MapNode {
		return nil
	}
	for _, p := range n.Pairs {
		if p.Key.Value == key {
			return p.Value
		}
	}
	return nil
}

// KeyNode returns the key node of a mapping's entry, for its position, or nil.
func (n *Node) KeyNode(key string) *Node {
	if n == nil || n.Kind != MapNode {
		return nil
	}
	for _, p := range n.Pairs {
		if p.Key.Value == key {
			return p.Key
		}
	}
	return nil
}

// Error is a problem at a place in the document.
type Error struct {
	Line   int
	Column int // It is 0 when only the line is known.
	Msg    string
}

func (e *Error) Error() string {
	if e.Column > 0 {
		return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Column, e.Msg)
	}
	return fmt.Sprintf("line %d: %s", e.Line, e.Msg)
}

func errorAt(line int, format string, args ...any) *Error {
	return &Error{Line: line, Msg: fmt.Sprintf(format, args...)}
}

// Parse parses one YAML document. An empty document is a null scalar.
func Parse(src []byte) (*Node, error) {
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	if !utf8.ValidString(text) {
		return nil, errorAt(1, "the document isn't UTF-8")
	}
	text = strings.TrimPrefix(text, "\ufeff")
	p := &parser{lines: strings.Split(text, "\n")}
	for i, l := range p.lines {
		indent := len(l) - len(strings.TrimLeft(l, " \t"))
		if strings.Contains(l[:indent], "\t") && strings.TrimSpace(l) != "" {
			return nil, errorAt(i+1, "tabs aren't allowed for indentation")
		}
	}
	// The document may start with one document start marker, ---.
	if i := p.nextContent(0); i < len(p.lines) && isMarker(p.lines[i], "---") {
		p.lines[i] = ""
	}
	start := p.nextContent(0)
	if start >= len(p.lines) {
		return &Node{Kind: ScalarNode, Line: 1, Column: 1}, nil
	}
	p.i = start
	n, err := p.block(indentOf(p.lines[start]))
	if err != nil {
		return nil, err
	}
	if i := p.nextContent(p.i); i < len(p.lines) {
		if isMarker(p.lines[i], "---") || isMarker(p.lines[i], "...") {
			return nil, errorAt(i+1, "only one document is supported")
		}
		return nil, errorAt(i+1, "bad indentation, or content after the document")
	}
	return n, nil
}

type parser struct {
	lines []string
	i     int // It is the index of the next line to read.
}

func isMarker(line, marker string) bool {
	rest, ok := strings.CutPrefix(line, marker)
	return ok && (rest == "" || rest[0] == ' ' || rest[0] == '#')
}

func indentOf(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

// blank reports whether a line holds nothing but spaces and a comment.
func blank(line string) bool {
	t := strings.TrimLeft(line, " \t")
	return t == "" || t[0] == '#'
}

// nextContent is the index of the first line from i that isn't blank.
func (p *parser) nextContent(i int) int {
	for i < len(p.lines) && blank(p.lines[i]) {
		i++
	}
	return i
}

// block parses the node starting at p.i, whose first line is indented by indent.
func (p *parser) block(indent int) (*Node, error) {
	line := p.lines[p.i]
	rest := line[indent:]
	switch {
	case rest == "-" || strings.HasPrefix(rest, "- ") || strings.HasPrefix(rest, "-#"):
		return p.sequence(indent)
	case strings.HasPrefix(rest, "? "), rest == "?":
		return nil, errorAt(p.i+1, "complex keys aren't supported")
	}
	if _, _, ok, err := p.splitKey(p.i, indent); err != nil {
		return nil, err
	} else if ok {
		return p.mapping(indent)
	}
	// The node is a lone value: the whole document, or a value on the line after its key.
	n, err := p.inline(p.i, indent)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// splitKey reads a mapping key at the start of line i, returning the key and the column
// its value starts at. ok is false when the line isn't a mapping entry.
func (p *parser) splitKey(i, indent int) (key *Node, valueCol int, ok bool, err error) {
	line := p.lines[i]
	rest := line[indent:]
	if rest == "" {
		return nil, 0, false, nil
	}
	switch rest[0] {
	case '"', '\'':
		s := &scanner{lines: p.lines, line: i, col: indent}
		k, err := s.quoted()
		if err != nil {
			return nil, 0, false, err
		}
		if s.line != i || !s.at(":") {
			return nil, 0, false, nil
		}
		s.col++
		if s.col < len(line) && line[s.col] != ' ' {
			return nil, 0, false, nil
		}
		return k, s.col, true, nil
	case '[', '{', '|', '>', '#':
		return nil, 0, false, nil
	}
	for j := indent; j < len(line); j++ {
		c := line[j]
		if c == '#' && j > indent && line[j-1] == ' ' {
			return nil, 0, false, nil // A comment starts before any key separator.
		}
		if c == ':' && (j+1 == len(line) || line[j+1] == ' ') {
			k := strings.TrimRight(line[indent:j], " ")
			if k == "<<" {
				return nil, 0, false, errorAt(i+1, "merge keys aren't supported")
			}
			return &Node{Kind: ScalarNode, Value: k, Line: i + 1, Column: indent + 1}, j + 1, true, nil
		}
	}
	return nil, 0, false, nil
}

func (p *parser) mapping(indent int) (*Node, error) {
	m := &Node{Kind: MapNode, Line: p.i + 1, Column: indent + 1}
	seen := map[string]bool{}
	for {
		p.i = p.nextContent(p.i)
		if p.i >= len(p.lines) {
			return m, nil
		}
		line := p.lines[p.i]
		ind := indentOf(line)
		if ind < indent || isMarker(line, "---") || isMarker(line, "...") {
			return m, nil
		}
		if ind > indent {
			return nil, errorAt(p.i+1, "bad indentation")
		}
		key, valueCol, ok, err := p.splitKey(p.i, indent)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errorAt(p.i+1, "expected a mapping key, as `key: value`")
		}
		if seen[key.Value] {
			return nil, errorAt(p.i+1, "duplicate key %q", key.Value)
		}
		seen[key.Value] = true
		value, err := p.value(p.i, valueCol, indent, true)
		if err != nil {
			return nil, err
		}
		m.Pairs = append(m.Pairs, Pair{Key: key, Value: value})
	}
}

// value parses what follows a key's colon or a sequence item's dash, at line i from col.
// It is on the same line, or the indented block below. Under a mapping key, a sequence
// may sit at the key's own indentation.
func (p *parser) value(i, col, parentIndent int, underKey bool) (*Node, error) {
	line := p.lines[i]
	rest := strings.TrimLeft(line[col:], " ")
	start := len(line) - len(rest)
	if rest == "" || rest[0] == '#' {
		next := p.nextContent(i + 1)
		p.i = i + 1
		if next < len(p.lines) {
			nl := p.lines[next]
			ind := indentOf(nl)
			isSeq := strings.HasPrefix(nl[ind:], "- ") || nl[ind:] == "-"
			if ind > parentIndent || (underKey && ind == parentIndent && isSeq) {
				p.i = next
				return p.block(ind)
			}
		}
		return &Node{Kind: ScalarNode, Line: i + 1, Column: col + 1}, nil
	}
	if rest[0] == '|' || rest[0] == '>' {
		return p.blockScalar(i, start, parentIndent)
	}
	n, err := p.inline(i, start)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// inline parses a value that starts at line i, column col: a flow collection, a quoted
// scalar or a plain scalar. It leaves p.i at the line after the value.
func (p *parser) inline(i, col int) (*Node, error) {
	s := &scanner{lines: p.lines, line: i, col: col}
	n, err := s.flowValue(false)
	if err != nil {
		return nil, err
	}
	if n.Kind == ScalarNode && n.Style == Plain && strings.Contains(n.Value, ": ") {
		return nil, errorAt(i+1, "a mapping value isn't allowed here: quote the value if it holds ': '")
	}
	if err := s.endOfLine(); err != nil {
		return nil, err
	}
	p.i = s.line + 1
	return n, nil
}

func (p *parser) sequence(indent int) (*Node, error) {
	seq := &Node{Kind: SeqNode, Line: p.i + 1, Column: indent + 1}
	for {
		p.i = p.nextContent(p.i)
		if p.i >= len(p.lines) {
			return seq, nil
		}
		line := p.lines[p.i]
		ind := indentOf(line)
		if ind < indent {
			return seq, nil
		}
		if ind > indent {
			return nil, errorAt(p.i+1, "bad indentation")
		}
		rest := line[indent:]
		if !(rest == "-" || strings.HasPrefix(rest, "- ") || strings.HasPrefix(rest, "-#")) {
			return seq, nil // The line is the rest of a mapping that holds this sequence.
		}
		content := strings.TrimLeft(rest[1:], " ")
		if content == "" || content[0] == '#' {
			item, err := p.value(p.i, indent+1, indent, false)
			if err != nil {
				return nil, err
			}
			seq.Items = append(seq.Items, item)
			continue
		}
		// The item starts on the dash's line: blank the dash out and parse the item as a
		// block indented to where its content starts, so `- name: a` continues with
		// `  size: 1` below it, and `- - x` nests.
		itemIndent := len(line) - len(content)
		p.lines[p.i] = strings.Repeat(" ", itemIndent) + content
		item, err := p.block(itemIndent)
		if err != nil {
			return nil, err
		}
		seq.Items = append(seq.Items, item)
	}
}

// blockScalar parses | and > scalars, whose header is at line i, column col.
func (p *parser) blockScalar(i, col, parentIndent int) (*Node, error) {
	line := p.lines[i]
	header := line[col:]
	if c := strings.Index(header, " #"); c >= 0 {
		header = header[:c]
	}
	header = strings.TrimRight(header, " ")
	style := Literal
	if header[0] == '>' {
		style = Folded
	}
	chomp := header[1:]
	if chomp != "" && chomp != "-" && chomp != "+" {
		return nil, errorAt(i+1, "unsupported block scalar header %q", header)
	}
	n := &Node{Kind: ScalarNode, Style: style, Line: i + 1, Column: col + 1}

	var body []string
	blockIndent := -1
	j := i + 1
	for ; j < len(p.lines); j++ {
		l := p.lines[j]
		if strings.TrimSpace(l) == "" {
			body = append(body, "")
			continue
		}
		ind := indentOf(l)
		if blockIndent < 0 {
			if ind <= parentIndent {
				break
			}
			blockIndent = ind
		}
		if ind < blockIndent {
			break
		}
		body = append(body, l[blockIndent:])
	}
	// Trailing blank lines belong to the scalar only for keep chomping.
	trailing := 0
	for k := len(body) - 1; k >= 0 && body[k] == ""; k-- {
		trailing++
	}
	content := body[:len(body)-trailing]
	p.i = j

	var text string
	if style == Literal {
		text = strings.Join(content, "\n")
	} else {
		// When folding, lines join with a space, each blank line is a line break, and more
		// indented lines keep their breaks.
		var b strings.Builder
		for k, l := range content {
			if k > 0 {
				prev := content[k-1]
				switch {
				case l == "":
					b.WriteString("\n")
				case prev == "":
				case strings.HasPrefix(l, " ") || strings.HasPrefix(prev, " "):
					b.WriteString("\n")
				default:
					b.WriteString(" ")
				}
			}
			b.WriteString(l)
		}
		text = b.String()
	}
	switch {
	case len(content) == 0:
	case chomp == "-":
	case chomp == "+":
		text += strings.Repeat("\n", 1+trailing)
	default:
		text += "\n"
	}
	n.Value = text
	return n, nil
}

// scanner reads flow values character by character. Flow collections may span lines.
type scanner struct {
	lines []string
	line  int
	col   int
}

func (s *scanner) cur() string { return s.lines[s.line] }

func (s *scanner) at(prefix string) bool {
	return s.line < len(s.lines) && strings.HasPrefix(s.cur()[s.col:], prefix)
}

func (s *scanner) peek() byte {
	if s.line >= len(s.lines) || s.col >= len(s.cur()) {
		return 0
	}
	return s.cur()[s.col]
}

func (s *scanner) errorf(format string, args ...any) *Error {
	return errorAt(s.line+1, format, args...)
}

// skipSpace skips spaces and a comment on this line, and with multiline set, as in a flow
// collection, line ends too.
func (s *scanner) skipSpace(multiline bool) {
	for s.line < len(s.lines) {
		l := s.cur()
		for s.col < len(l) && (l[s.col] == ' ' || l[s.col] == '\t') {
			s.col++
		}
		if s.col < len(l) && l[s.col] == '#' && (s.col == 0 || l[s.col-1] == ' ' || l[s.col-1] == '\t') {
			s.col = len(l)
		}
		if s.col < len(l) || !multiline || s.line+1 >= len(s.lines) {
			return
		}
		s.line++
		s.col = 0
	}
}

// endOfLine checks that nothing but a comment follows the value.
func (s *scanner) endOfLine() error {
	s.skipSpace(false)
	if s.col < len(s.cur()) {
		return &Error{Line: s.line + 1, Column: s.col + 1, Msg: fmt.Sprintf("unexpected %q after the value", s.cur()[s.col:])}
	}
	return nil
}

// flowValue reads one value. inFlow is true inside a flow collection, where , ] and }
// end a plain scalar.
func (s *scanner) flowValue(inFlow bool) (*Node, error) {
	s.skipSpace(inFlow)
	line, col := s.line+1, s.col+1
	switch c := s.peek(); c {
	case '[':
		return s.flowSeq()
	case '{':
		return s.flowMap()
	case '"', '\'':
		return s.quoted()
	case '&':
		return nil, s.errorf("anchors aren't supported")
	case '*':
		return nil, s.errorf("aliases aren't supported")
	case '!':
		return nil, s.errorf("tags aren't supported")
	case '|', '>':
		if inFlow {
			return nil, s.errorf("block scalars aren't allowed in a flow collection")
		}
	case '@', '`':
		return nil, s.errorf("%q can't start a plain scalar", c)
	}
	v := s.plain(inFlow)
	return &Node{Kind: ScalarNode, Value: v, Line: line, Column: col}, nil
}

// plain reads a plain scalar to the end of the line, a comment, or in a flow collection
// a , ] } or a key's colon.
func (s *scanner) plain(inFlow bool) string {
	l := s.cur()
	start := s.col
	end := s.col
	for i := s.col; i < len(l); i++ {
		c := l[i]
		if c == '#' && i > start && (l[i-1] == ' ' || l[i-1] == '\t') {
			break
		}
		if inFlow {
			if c == ',' || c == ']' || c == '}' {
				break
			}
			if c == ':' && (i+1 == len(l) || strings.ContainsRune(" ,]}", rune(l[i+1]))) {
				break
			}
		}
		end = i + 1
	}
	s.col = end
	return strings.TrimRight(l[start:end], " \t")
}

func (s *scanner) flowSeq() (*Node, error) {
	n := &Node{Kind: SeqNode, Line: s.line + 1, Column: s.col + 1}
	open := s.line
	s.col++ // This skips the [.
	for {
		s.skipSpace(true)
		switch s.peek() {
		case ']':
			s.col++
			return n, nil
		case 0:
			return nil, errorAt(open+1, "unclosed [")
		}
		item, err := s.flowValue(true)
		if err != nil {
			return nil, err
		}
		if item.Kind == ScalarNode && item.Style == Plain && item.Value == "" {
			return nil, errorAt(open+1, "unclosed [, or an empty entry")
		}
		n.Items = append(n.Items, item)
		s.skipSpace(true)
		switch s.peek() {
		case ',':
			s.col++
		case ']':
		default:
			return nil, errorAt(open+1, "unclosed [: expected , or ]")
		}
	}
}

func (s *scanner) flowMap() (*Node, error) {
	n := &Node{Kind: MapNode, Line: s.line + 1, Column: s.col + 1}
	open := s.line
	seen := map[string]bool{}
	s.col++ // This skips the {.
	for {
		s.skipSpace(true)
		switch s.peek() {
		case '}':
			s.col++
			return n, nil
		case 0:
			return nil, errorAt(open+1, "unclosed {")
		}
		key, err := s.flowValue(true)
		if err != nil {
			return nil, err
		}
		if key.Kind != ScalarNode {
			return nil, s.errorf("a mapping key must be a scalar")
		}
		s.skipSpace(true)
		if s.peek() != ':' {
			return nil, s.errorf("expected : after the key %q", key.Value)
		}
		s.col++
		if seen[key.Value] {
			return nil, errorAt(key.Line, "duplicate key %q", key.Value)
		}
		seen[key.Value] = true
		var value *Node
		s.skipSpace(true)
		if c := s.peek(); c == ',' || c == '}' {
			value = &Node{Kind: ScalarNode, Line: s.line + 1, Column: s.col + 1}
		} else if value, err = s.flowValue(true); err != nil {
			return nil, err
		}
		n.Pairs = append(n.Pairs, Pair{Key: key, Value: value})
		s.skipSpace(true)
		switch s.peek() {
		case ',':
			s.col++
		case '}':
		case 0:
			return nil, errorAt(open+1, "unclosed {")
		default:
			return nil, s.errorf("expected , or } in a flow mapping")
		}
	}
}

// quoted reads a single- or double-quoted scalar, which must end on its line.
func (s *scanner) quoted() (*Node, error) {
	l := s.cur()
	q := l[s.col]
	n := &Node{Kind: ScalarNode, Style: SingleQuoted, Line: s.line + 1, Column: s.col + 1}
	if q == '"' {
		n.Style = DoubleQuoted
	}
	var b strings.Builder
	i := s.col + 1
	for {
		if i >= len(l) {
			return nil, s.errorf("unclosed quote")
		}
		c := l[i]
		switch {
		case q == '\'' && c == '\'':
			if i+1 < len(l) && l[i+1] == '\'' {
				b.WriteByte('\'')
				i += 2
				continue
			}
			n.Value = b.String()
			s.col = i + 1
			return n, nil
		case q == '"' && c == '"':
			n.Value = b.String()
			s.col = i + 1
			return n, nil
		case q == '"' && c == '\\':
			r, width, err := unescape(l[i+1:])
			if err != nil {
				return nil, s.errorf("%v", err)
			}
			b.WriteString(r)
			i += 1 + width
		default:
			b.WriteByte(c)
			i++
		}
	}
}

func unescape(s string) (string, int, error) {
	if s == "" {
		return "", 0, fmt.Errorf("unfinished escape")
	}
	simple := map[byte]string{'0': "\x00", 'a': "\a", 'b': "\b", 't': "\t", 'n': "\n", 'v': "\v",
		'f': "\f", 'r': "\r", 'e': "\x1b", ' ': " ", '"': `"`, '/': "/", '\\': `\`, 'N': "\u0085",
		'_': " ", 'L': " ", 'P': " "}
	if r, ok := simple[s[0]]; ok {
		return r, 1, nil
	}
	width := map[byte]int{'x': 2, 'u': 4, 'U': 8}[s[0]]
	if width == 0 || len(s) < 1+width {
		return "", 0, fmt.Errorf("unknown escape \\%c", s[0])
	}
	v, err := strconv.ParseUint(s[1:1+width], 16, 32)
	if err != nil {
		return "", 0, fmt.Errorf("bad escape \\%s", s[:1+width])
	}
	return string(rune(v)), 1 + width, nil
}
