package client

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpTool is a tool from GET /api/mcp/servers/{name}/tools.
type mcpTool struct {
	Name         string               `json:"name"`
	Title        string               `json:"title"`
	Description  string               `json:"description"`
	Annotations  *sdk.ToolAnnotations `json:"annotations"`
	InputSchema  *schema              `json:"inputSchema"`
	OutputSchema *schema              `json:"outputSchema"`

	raw json.RawMessage // as the server sent it, for -json and -schema
}

func (t *mcpTool) UnmarshalJSON(b []byte) error {
	type plain mcpTool
	if err := json.Unmarshal(b, (*plain)(t)); err != nil {
		return err
	}
	t.InputSchema, t.OutputSchema = resolveRefs(t.InputSchema), resolveRefs(t.OutputSchema)
	t.raw = slices.Clone(b)
	return nil
}

func (t *mcpTool) MarshalJSON() ([]byte, error) { return t.raw, nil }

// schema is the part of a JSON Schema needed to render TypeScript types and
// to build arguments from KEY=VALUE pairs.
type schema struct {
	Type                 schemaTypes        `json:"type"`
	Enum                 []json.RawMessage  `json:"enum"`
	Const                json.RawMessage    `json:"const"`
	AnyOf                []*schema          `json:"anyOf"`
	OneOf                []*schema          `json:"oneOf"`
	Items                *schema            `json:"items"`
	Properties           map[string]*schema `json:"properties"`
	Required             []string           `json:"required"`
	AdditionalProperties *schema            `json:"additionalProperties"`
	Title                string             `json:"title"`
	Description          string             `json:"description"`
	Default              json.RawMessage    `json:"default"`
	Ref                  string             `json:"$ref"`
	Defs                 map[string]*schema `json:"$defs"`
	Definitions          map[string]*schema `json:"definitions"`

	cut string // the name of the definition a recursive reference refers to
}

// UnmarshalJSON never fails: a boolean schema, or one with a keyword of an
// unexpected shape, accepts anything as far as we know, and mustn't break
// the listing of other tools.
func (s *schema) UnmarshalJSON(b []byte) error {
	type plain schema
	if json.Unmarshal(b, (*plain)(s)) != nil {
		*s = schema{}
	}
	return nil
}

// schemaTypes is the "type" keyword: a name or a list of names.
type schemaTypes []string

func (t *schemaTypes) UnmarshalJSON(b []byte) error {
	var name string
	if json.Unmarshal(b, &name) == nil {
		*t = schemaTypes{name}
		return nil
	}
	return json.Unmarshal(b, (*[]string)(t))
}

// resolveRefs returns a copy of s in which references to its definitions
// ("#/$defs/NAME", "#/definitions/NAME") are replaced by the definitions. A
// reference inside the definition it refers to (a recursive type) renders as
// NAME.
func resolveRefs(s *schema) *schema {
	if s == nil {
		return nil
	}
	defs := map[string]*schema{}
	for name, d := range s.Defs {
		defs["#/$defs/"+name] = d
	}
	for name, d := range s.Definitions {
		defs["#/definitions/"+name] = d
	}
	active := map[string]bool{}
	var resolve func(*schema) *schema
	resolveAll := func(ss []*schema) []*schema {
		var out []*schema
		for _, s := range ss {
			out = append(out, resolve(s))
		}
		return out
	}
	resolve = func(s *schema) *schema {
		if s == nil {
			return nil
		}
		c := *s
		if d := defs[s.Ref]; d != nil {
			if active[s.Ref] {
				c.cut = s.Ref[strings.LastIndex(s.Ref, "/")+1:]
				return &c
			}
			active[s.Ref] = true
			r := resolve(d)
			delete(active, s.Ref)
			r.Description = cmp.Or(s.Description, r.Description)
			if s.Default != nil {
				r.Default = s.Default
			}
			return r
		}
		c.Items, c.AdditionalProperties = resolve(s.Items), resolve(s.AdditionalProperties)
		c.AnyOf, c.OneOf = resolveAll(s.AnyOf), resolveAll(s.OneOf)
		c.Properties = map[string]*schema{}
		for name, p := range s.Properties {
			c.Properties[name] = resolve(p)
		}
		return &c
	}
	return resolve(s)
}

type param struct {
	name     string
	schema   *schema
	required bool
}

// params returns the properties of an object schema: required ones first,
// then optional ones, each by name. (The server's order is lost by the time
// the tools reach us.)
func (s *schema) params() []param {
	if s == nil {
		return nil
	}
	var req, opt []param
	for _, name := range slices.Sorted(maps.Keys(s.Properties)) {
		p := param{name, s.Properties[name], slices.Contains(s.Required, name)}
		if p.required {
			req = append(req, p)
		} else {
			opt = append(opt, p)
		}
	}
	return append(req, opt...)
}

// decl renders p as "name: T" or "name?: T".
func (p param) decl() string {
	if p.required {
		return p.name + ": " + p.schema.ts()
	}
	return p.name + "?: " + p.schema.ts()
}

// ts renders s as a TypeScript type.
func (s *schema) ts() string {
	return strings.Join(s.union(), " | ")
}

// union renders s as the members of a TypeScript union.
func (s *schema) union() []string {
	var ts []string
	switch {
	case s == nil:
		return []string{"unknown"}
	case s.cut != "":
		return []string{s.cut}
	case s.Const != nil:
		return []string{compactJSON(s.Const)}
	case len(s.Enum) > 0:
		for _, v := range s.Enum {
			ts = append(ts, compactJSON(v))
		}
	case len(s.AnyOf)+len(s.OneOf) > 0:
		for _, a := range slices.Concat(s.AnyOf, s.OneOf) {
			ts = append(ts, a.union()...)
		}
	case len(s.Type) == 0 && len(s.Properties) > 0:
		return []string{s.object()}
	case len(s.Type) == 0:
		return []string{"unknown"}
	default:
		for _, typ := range s.Type {
			ts = append(ts, s.typeName(typ))
		}
	}
	var u []string
	for _, t := range ts {
		if !slices.Contains(u, t) {
			u = append(u, t)
		}
	}
	return u
}

// typeName renders s as the TypeScript type for the JSON type typ.
func (s *schema) typeName(typ string) string {
	switch typ {
	case "string", "boolean", "null":
		return typ
	case "number", "integer":
		return "number"
	case "array":
		if items := s.Items.union(); len(items) > 1 {
			return "(" + strings.Join(items, " | ") + ")[]"
		}
		return s.Items.ts() + "[]"
	case "object":
		return s.object()
	}
	return "unknown"
}

func (s *schema) object() string {
	params := s.params()
	if len(params) == 0 {
		if t := s.AdditionalProperties.ts(); t != "unknown" {
			return "Record<string, " + t + ">"
		}
		return "object"
	}
	var fields []string
	for _, p := range params {
		fields = append(fields, p.decl())
	}
	return "{ " + strings.Join(fields, "; ") + " }"
}

func compactJSON(b json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, b) != nil {
		return string(b)
	}
	return buf.String()
}

// signature renders t as `function NAME(params): RETURN;`.
func (t *mcpTool) signature() string {
	var decls []string
	for _, p := range t.InputSchema.params() {
		decls = append(decls, p.decl())
	}
	sig := "function " + t.Name + "(" + strings.Join(decls, ", ") + ")"
	if t.OutputSchema != nil && t.OutputSchema.Title != "" {
		sig += ": " + t.OutputSchema.Title
	}
	return sig + ";"
}

// doc renders t as a doc comment and its signature.
func (t *mcpTool) doc() string {
	var lines []string
	if d := strings.TrimSpace(t.Description); d != "" {
		lines = append(lines, d)
	}
	a := cmp.Or(t.Annotations, &sdk.ToolAnnotations{})
	if title := cmp.Or(t.Title, a.Title); title != "" && title != t.Name {
		lines = append(lines, "@title "+title)
	}
	if a.ReadOnlyHint {
		lines = append(lines, "@readOnly")
	} else if a.DestructiveHint != nil && *a.DestructiveHint {
		lines = append(lines, "@destructive")
	}
	for _, p := range t.InputSchema.params() {
		if p.schema == nil {
			continue
		}
		d := p.schema.Description
		if p.schema.Default != nil && string(p.schema.Default) != "null" {
			d += " (default: " + compactJSON(p.schema.Default) + ")"
		}
		if d = strings.TrimSpace(d); d != "" {
			lines = append(lines, "@param "+p.name+" "+d)
		}
	}
	var b strings.Builder
	if len(lines) > 0 {
		b.WriteString("/**\n")
		for _, l := range strings.Split(strings.Join(lines, "\n"), "\n") {
			b.WriteString(strings.TrimRight(" * "+l, " \t\r") + "\n")
		}
		b.WriteString(" */\n")
	}
	return b.String() + t.signature() + "\n"
}

// arguments builds t's arguments from KEY=VALUE pairs: VALUE is the string
// itself for a string parameter, else JSON.
func (t *mcpTool) arguments(sel string, pairs []string) (map[string]json.RawMessage, error) {
	params := t.InputSchema.params()
	args := map[string]json.RawMessage{}
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, usageError(fmt.Sprintf("%q is not KEY=VALUE", pair))
		}
		i := slices.IndexFunc(params, func(p param) bool { return p.name == key })
		switch {
		case i < 0:
			return nil, fmt.Errorf("%s has no parameter %q\n%s", sel, key, t.signature())
		case params[i].schema.stringOnly():
			args[key], _ = json.Marshal(value)
		case json.Valid([]byte(value)):
			args[key] = json.RawMessage(value)
		default:
			return nil, fmt.Errorf("%s: %s is %s, so its value must be JSON, quoted for the shell: '%s=...'\n%s",
				sel, key, params[i].schema.ts(), key, t.signature())
		}
	}
	var missing []string
	for _, p := range params {
		if _, ok := args[p.name]; p.required && !ok {
			missing = append(missing, p.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s: missing %s\n%s", sel, strings.Join(missing, ", "), t.signature())
	}
	return args, nil
}

// stringOnly reports whether s accepts strings and perhaps null, judging by
// its TypeScript type.
func (s *schema) stringOnly() bool {
	str := false
	for _, t := range s.union() {
		switch {
		case t == "null":
		case t == "string" || json.Unmarshal([]byte(t), new(string)) == nil:
			str = true
		default:
			return false
		}
	}
	return str
}
