package client

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func mustTool(t *testing.T, s string) *mcpTool {
	t.Helper()
	var tool mcpTool
	if err := json.Unmarshal([]byte(s), &tool); err != nil {
		t.Fatalf("%s: %v", s, err)
	}
	return &tool
}

func TestSchemaTypes(t *testing.T) {
	for _, tc := range []struct{ schema, want string }{
		{`{"type":"string"}`, "string"},
		{`{"type":"integer"}`, "number"},
		{`{"type":"boolean"}`, "boolean"},
		{`{"type":"null"}`, "null"},
		{`{"type":"string","enum":["a","b"]}`, `"a" | "b"`},
		{`{"enum":[1,"x",null]}`, `1 | "x" | null`},
		{`{"const":"fixed"}`, `"fixed"`},
		{`{"type":"array","items":{"type":"string"}}`, "string[]"},
		{`{"type":"array","items":{"type":["string","number"]}}`, "(string | number)[]"},
		{`{"type":"array","items":{"type":"object","properties":{"k":{"enum":["a","b"]}}}}`, `{ k?: "a" | "b" }[]`},
		{`{"type":"array"}`, "unknown[]"},
		{`{"type":"object"}`, "object"},
		{`{"type":"object","additionalProperties":false}`, "object"},
		{`{"type":"object","additionalProperties":{"type":"number"}}`, "Record<string, number>"},
		{`{"type":"object","properties":{"z":{"type":"number"},"b":{"type":"object","properties":{"x":{"type":"boolean"}},"required":["x"]},"a":{}},"required":["z"]}`, "{ z: number; a?: unknown; b?: { x: boolean } }"},
		{`{"properties":{"a":{"type":"string"}}}`, "{ a?: string }"},
		{`{"type":["string","null"]}`, "string | null"},
		{`{"anyOf":[{"type":"string"},{"type":"null"}]}`, "string | null"},
		{`{"oneOf":[{"type":"integer"},{"type":"number"},{"type":"string"}]}`, "number | string"},
		{`{"type":5,"properties":{"a":{}}}`, "unknown"},
		{`{}`, "unknown"},
		{`true`, "unknown"},
	} {
		var s schema
		if err := json.Unmarshal([]byte(tc.schema), &s); err != nil {
			t.Fatal(err)
		}
		if got := s.ts(); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.schema, got, tc.want)
		}
	}
}

func TestToolDoc(t *testing.T) {
	tool := mustTool(t, `{
		"name": "create_issue",
		"title": "Create issue",
		"description": "Create an issue.\n\nMarkdown is fine.\n",
		"annotations": {"destructiveHint": true},
		"inputSchema": {
			"type": "object",
			"properties": {
				"labels": {"type": "array", "items": {"type": "string"}, "description": "Labels"},
				"title": {"type": "string", "description": "The title"},
				"priority": {"type": "integer", "default": 3, "description": "1 is urgent\n5 is someday"},
				"team": {"type": "string"},
				"state": {"enum": ["open", "closed"], "default": "open"}
			},
			"required": ["title", "team"]
		},
		"outputSchema": {"title": "Issue", "type": "object"}
	}`)
	want := `/**
 * Create an issue.
 *
 * Markdown is fine.
 * @title Create issue
 * @destructive
 * @param title The title
 * @param labels Labels
 * @param priority 1 is urgent
 * 5 is someday (default: 3)
 * @param state (default: "open")
 */
function create_issue(team: string, title: string, labels?: string[], priority?: number, state?: "open" | "closed"): Issue;
`
	if got := tool.doc(); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}

	for _, tc := range []struct{ tool, want string }{
		{`{"name":"pid","inputSchema":{"type":"object"}}`, "function pid();\n"},
		{`{"name":"same","title":"same","annotations":{"readOnlyHint":true,"destructiveHint":true}}`, "/**\n * @readOnly\n */\nfunction same();\n"},
		{`{"name":"t","annotations":{"title":"From annotations"}}`, "/**\n * @title From annotations\n */\nfunction t();\n"},
		{ // Pydantic: an enum in $defs, referred to with a description and a default
			`{"name":"b","inputSchema":{"type":"object","properties":{"c":{"$ref":"#/$defs/Color","description":"Pick one","default":"red"},"cs":{"type":"array","items":{"$ref":"#/$defs/Color"}}},"required":["c"],"$defs":{"Color":{"type":"string","enum":["red","blue"],"description":"A color"}}}}`,
			"/**\n * @param c Pick one (default: \"red\")\n */\nfunction b(c: \"red\" | \"blue\", cs?: (\"red\" | \"blue\")[]);\n",
		},
		{ // recursive types are cut at the cycle; other references are unknown
			`{"name":"r","inputSchema":{"type":"object","properties":{"tree":{"$ref":"#/definitions/Node"},"far":{"$ref":"https://example.com/s.json"}},"definitions":{"Node":{"type":"object","properties":{"value":{"type":"string"},"kids":{"type":"array","items":{"$ref":"#/definitions/Node"}}}}}}}`,
			"function r(far?: unknown, tree?: { kids?: Node[]; value?: string });\n",
		},
		{ // mutually recursive definitions
			`{"name":"m","inputSchema":{"type":"object","properties":{"a":{"$ref":"#/$defs/A"},"b":{"$ref":"#/$defs/B"}},"$defs":{"A":{"type":"object","properties":{"b":{"$ref":"#/$defs/B"}}},"B":{"type":"object","properties":{"a":{"$ref":"#/$defs/A"}}}}}}`,
			"function m(a?: { b?: { a?: A } }, b?: { a?: { b?: B } });\n",
		},
	} {
		if got := mustTool(t, tc.tool).doc(); got != tc.want {
			t.Errorf("%s: got:\n%s\nwant:\n%s", tc.tool, got, tc.want)
		}
	}
}

func TestToolArguments(t *testing.T) {
	tool := mustTool(t, `{"name":"t","inputSchema":{"type":"object","properties":{
		"s": {"type":"string"},
		"ns": {"type":["string","null"]},
		"opt": {"anyOf":[{"type":"string"},{"type":"null"}]},
		"e": {"enum":["a","b"]},
		"c": {"$ref":"#/$defs/Color"},
		"k": {"const":"fixed"},
		"n": {"type":"integer"},
		"sn": {"type":["string","number"]},
		"ka": {"type":"array","items":{"const":"x"}},
		"obj": {"type":"object"},
		"any": {}
	},"required":["s"],"$defs":{"Color":{"type":"string","enum":["red","blue"]}}}}`)
	for _, tc := range []struct {
		pairs []string
		want  string
	}{
		// Strings are taken as is.
		{[]string{"s=42", "ns=null", "opt={}", "e=a", "c=red", "k=fixed"}, `{"c":"red","e":"a","k":"fixed","ns":"null","opt":"{}","s":"42"}`},
		{[]string{"s=a=b"}, `{"s":"a=b"}`},
		{[]string{"s="}, `{"s":""}`},
		// Everything else is JSON.
		{[]string{"s=x", "n=42", "sn=42", "ka=[\"x\"]", `obj={"k": [1]}`, "any=true"}, `{"any":true,"ka":["x"],"n":42,"obj":{"k":[1]},"s":"x","sn":42}`},
		{[]string{"s=x", `sn="42"`}, `{"s":"x","sn":"42"}`},
	} {
		args, err := tool.arguments("srv.t", tc.pairs)
		if err != nil {
			t.Fatalf("%q: %v", tc.pairs, err)
		}
		if b, _ := json.Marshal(args); string(b) != tc.want {
			t.Errorf("%q: got %s, want %s", tc.pairs, b, tc.want)
		}
	}

	sig := tool.signature()
	for _, tc := range []struct {
		pairs []string
		want  string
	}{
		{[]string{"s=x", "nope=1"}, "srv.t has no parameter \"nope\"\n" + sig},
		{[]string{"s=x", "n=two"}, "srv.t: n is number, so its value must be JSON, quoted for the shell: 'n=...'\n" + sig},
		{[]string{"n=1"}, "srv.t: missing s\n" + sig},
		{[]string{"s"}, `"s" is not KEY=VALUE`},
	} {
		_, err := tool.arguments("srv.t", tc.pairs)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%q: got %v, want %s", tc.pairs, err, tc.want)
		}
	}
	if _, err := tool.arguments("srv.t", []string{"s"}); !errors.As(err, new(usageError)) {
		t.Errorf("no '=': %T, want a usage error", err)
	}
	if !strings.HasPrefix(sig, "function t(s: string, any?: unknown, c?: \"red\" | \"blue\", e?: ") {
		t.Errorf("signature: %s", sig)
	}
}
