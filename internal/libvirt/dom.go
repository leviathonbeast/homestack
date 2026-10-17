package libvirt

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
)

// A small, lossless XML tree, so the VM settings form can change a few
// parts of a definition without dropping anything it doesn't know about
// (passthrough devices, CPU pinning, Unraid's metadata, comments…).
// Names and attributes are kept exactly as written, prefixes included.

// Node is one element.
type Node struct {
	Name  string     // as written, e.g. "disk" or "homestack:vm"
	Attrs []xml.Attr // Name.Space holds the prefix as written
	Kids  []any      // *Node, xml.CharData, xml.Comment, xml.ProcInst or xml.Directive
}

// ParseDoc reads a document and returns its root element.
func ParseDoc(s string) (*Node, error) {
	dec := xml.NewDecoder(strings.NewReader(s))
	var stack []*Node
	var root *Node
	for {
		tok, err := dec.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &Node{Name: rawName(t.Name), Attrs: append([]xml.Attr(nil), t.Attr...)}
			if len(stack) == 0 {
				if root != nil {
					return nil, errors.New("more than one root element")
				}
				root = n
			} else {
				p := stack[len(stack)-1]
				p.Kids = append(p.Kids, n)
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1].Name != rawName(t.Name) {
				return nil, errors.New("mismatched end tag </" + rawName(t.Name) + ">")
			}
			stack = stack[:len(stack)-1]
		default:
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				switch t := tok.(type) {
				case xml.CharData:
					p.Kids = append(p.Kids, t.Copy())
				case xml.Comment:
					p.Kids = append(p.Kids, t.Copy())
				}
			}
		}
	}
	if root == nil || len(stack) > 0 {
		return nil, errors.New("incomplete XML")
	}
	return root, nil
}

func rawName(n xml.Name) string {
	if n.Space != "" {
		return n.Space + ":" + n.Local
	}
	return n.Local
}

// String writes the element and everything inside it.
func (n *Node) String() string {
	var b bytes.Buffer
	n.write(&b)
	b.WriteByte('\n')
	return b.String()
}

func (n *Node) write(b *bytes.Buffer) {
	b.WriteString("<" + n.Name)
	for _, a := range n.Attrs {
		b.WriteString(" " + rawName(a.Name) + "='" + escAttr(a.Value) + "'")
	}
	if len(n.Kids) == 0 {
		b.WriteString("/>")
		return
	}
	b.WriteByte('>')
	for _, k := range n.Kids {
		switch k := k.(type) {
		case *Node:
			k.write(b)
		case xml.CharData:
			b.WriteString(escText(string(k)))
		case xml.Comment:
			b.WriteString("<!--" + string(k) + "-->")
		}
	}
	b.WriteString("</" + n.Name + ">")
}

var (
	textEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\r", "&#xD;")
	attrEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "'", "&apos;", "\"", "&quot;", "\n", "&#xA;", "\r", "&#xD;", "\t", "&#x9;")
)

func escText(s string) string { return textEsc.Replace(s) }
func escAttr(s string) string { return attrEsc.Replace(s) }

// ---------- Reading ----------

// Children returns the child elements called name ("" = all).
func (n *Node) Children(name string) []*Node {
	if n == nil {
		return nil
	}
	var out []*Node
	for _, k := range n.Kids {
		if c, ok := k.(*Node); ok && (name == "" || c.Name == name) {
			out = append(out, c)
		}
	}
	return out
}

// Child returns the first child element called name, or nil.
func (n *Node) Child(name string) *Node {
	if n == nil {
		return nil
	}
	for _, k := range n.Kids {
		if c, ok := k.(*Node); ok && c.Name == name {
			return c
		}
	}
	return nil
}

// Path follows child names, e.g. Path("os", "loader").
func (n *Node) Path(names ...string) *Node {
	for _, name := range names {
		n = n.Child(name)
	}
	return n
}

// Attr returns an attribute's value ("" if it isn't there).
func (n *Node) Attr(name string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Attrs {
		if rawName(a.Name) == name {
			return a.Value
		}
	}
	return ""
}

// Text returns the element's text.
func (n *Node) Text() string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	for _, k := range n.Kids {
		if c, ok := k.(xml.CharData); ok {
			b.Write(c)
		}
	}
	return strings.TrimSpace(b.String())
}

// ---------- Changing ----------

// SetAttr sets an attribute, adding it if needed.
func (n *Node) SetAttr(name, value string) {
	for i := range n.Attrs {
		if rawName(n.Attrs[i].Name) == name {
			n.Attrs[i].Value = value
			return
		}
	}
	space, local, ok := strings.Cut(name, ":")
	if !ok {
		space, local = "", name
	}
	n.Attrs = append(n.Attrs, xml.Attr{Name: xml.Name{Space: space, Local: local}, Value: value})
}

// DelAttr removes an attribute.
func (n *Node) DelAttr(name string) {
	out := n.Attrs[:0]
	for _, a := range n.Attrs {
		if rawName(a.Name) != name {
			out = append(out, a)
		}
	}
	n.Attrs = out
}

// SetText replaces the element's content with text.
func (n *Node) SetText(s string) { n.Kids = []any{xml.CharData(s)} }

// Remove takes a child element out, with the whitespace before it.
func (n *Node) Remove(c *Node) {
	for i, k := range n.Kids {
		if k == c {
			start := i
			if i > 0 {
				if cd, ok := n.Kids[i-1].(xml.CharData); ok && strings.TrimSpace(string(cd)) == "" {
					start = i - 1
				}
			}
			n.Kids = append(n.Kids[:start], n.Kids[i+1:]...)
			return
		}
	}
}

// Add appends a child element, indented like its siblings, and returns it.
// After is an optional sibling to add it after (nil = at the end).
func (n *Node) Add(c *Node, after *Node) *Node {
	indent := n.childIndent()
	pos := len(n.Kids)
	if after != nil {
		for i, k := range n.Kids {
			if k == after {
				pos = i + 1
			}
		}
	} else if pos > 0 {
		// Before the whitespace that closes the parent.
		if cd, ok := n.Kids[pos-1].(xml.CharData); ok && strings.TrimSpace(string(cd)) == "" {
			pos--
		}
	}
	ins := []any{xml.CharData(indent), c}
	if len(n.Kids) == 0 {
		ins = append(ins, xml.CharData(strings.TrimSuffix(indent, "  ")))
	}
	n.Kids = append(n.Kids[:pos], append(ins, n.Kids[pos:]...)...)
	return c
}

// Ensure returns the child called name, adding an empty one if needed.
func (n *Node) Ensure(name string) *Node {
	if c := n.Child(name); c != nil {
		return c
	}
	return n.Add(&Node{Name: name}, nil)
}

// childIndent guesses the whitespace before child elements ("\n    ").
func (n *Node) childIndent() string {
	for i, k := range n.Kids {
		if _, ok := k.(*Node); ok && i > 0 {
			if cd, ok := n.Kids[i-1].(xml.CharData); ok && strings.TrimSpace(string(cd)) == "" {
				return string(cd)
			}
		}
	}
	return "\n  "
}

// El builds an element: El("source", "file", "/a.iso").
func El(name string, attrs ...string) *Node {
	n := &Node{Name: name}
	for i := 0; i+1 < len(attrs); i += 2 {
		n.SetAttr(attrs[i], attrs[i+1])
	}
	return n
}

// With adds children to a new element (no indentation; libvirt reformats
// the XML when it saves it anyway).
func (n *Node) With(kids ...*Node) *Node {
	for _, k := range kids {
		n.Kids = append(n.Kids, k)
	}
	return n
}
