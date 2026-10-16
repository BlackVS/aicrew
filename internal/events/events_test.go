package events

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// validate checks doc against the subset of JSON Schema the subscription
// schema uses: type, const, enum, required, properties,
// additionalProperties false, items, minItems, uniqueItems, pattern,
// maxLength and local $ref.
func validate(root, schema map[string]any, doc any, at string) []string {
	if ref, ok := schema["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/$defs/")
		return validate(root, root["$defs"].(map[string]any)[name].(map[string]any), doc, at)
	}
	var errs []string
	fail := func(f string, a ...any) { errs = append(errs, at+": "+fmt.Sprintf(f, a...)) }
	if c, ok := schema["const"]; ok && !reflect.DeepEqual(c, doc) {
		fail("is %v, not %v", doc, c)
	}
	if enum, ok := schema["enum"].([]any); ok && !slices.ContainsFunc(enum, func(v any) bool { return reflect.DeepEqual(v, doc) }) {
		fail("%v is not one of %v", doc, enum)
	}
	switch schema["type"] {
	case "object":
		obj, ok := doc.(map[string]any)
		if !ok {
			fail("is not an object")
			return errs
		}
		props, _ := schema["properties"].(map[string]any)
		for _, r := range schema["required"].([]any) {
			if _, ok := obj[r.(string)]; !ok {
				fail("lacks %s", r)
			}
		}
		for k, v := range obj {
			p, ok := props[k]
			if !ok {
				if schema["additionalProperties"] == false {
					fail("has %s, which the schema does not name", k)
				}
				continue
			}
			errs = append(errs, validate(root, p.(map[string]any), v, at+"."+k)...)
		}
	case "array":
		arr, ok := doc.([]any)
		if !ok {
			fail("is not an array")
			return errs
		}
		if n, ok := schema["minItems"].(float64); ok && len(arr) < int(n) {
			fail("has %d items, fewer than %v", len(arr), n)
		}
		if schema["uniqueItems"] == true {
			for i := range arr {
				for j := range i {
					if reflect.DeepEqual(arr[i], arr[j]) {
						fail("repeats %v", arr[i])
					}
				}
			}
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, v := range arr {
				errs = append(errs, validate(root, items, v, fmt.Sprintf("%s[%d]", at, i))...)
			}
		}
	case "string":
		s, ok := doc.(string)
		if !ok {
			fail("is not a string")
			return errs
		}
		if p, ok := schema["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(s) {
			fail("%q does not match %s", s, p)
		}
		if n, ok := schema["maxLength"].(float64); ok && len([]rune(s)) > int(n) {
			fail("is longer than %v", n)
		}
	}
	return errs
}

func schemaDoc(t *testing.T) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(SchemaJSON, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

// The release's subscriptions validate against the published schema, and
// Parse reads them.
func TestDefaultsMatchTheSchema(t *testing.T) {
	schema := schemaDoc(t)
	var doc any
	if err := json.Unmarshal(defaultsJSON, &doc); err != nil {
		t.Fatal(err)
	}
	if errs := validate(schema, schema, doc, "subscriptions.json"); len(errs) != 0 {
		t.Fatalf("the defaults do not match the schema:\n%s", strings.Join(errs, "\n"))
	}
	d := Defaults()
	if len(d.Subscriptions) != 2 || len(d.Version) != 16 || len(d.Matching(KindBoardChanged)) != 1 ||
		len(d.Matching(KindForgeChanged)) != 1 {
		t.Fatalf("the defaults: %+v", d)
	}
}

// The schema and Parse agree on what a file may name: the schema's enums
// are this release's kinds, roles, conditions and handlers.
func TestSchemaNamesTheReleasesVocabulary(t *testing.T) {
	defs := schemaDoc(t)["$defs"].(map[string]any)
	enum := func(path ...string) []string {
		var node any = defs
		for _, p := range path {
			node = node.(map[string]any)[p]
		}
		var out []string
		for _, v := range node.(map[string]any)["enum"].([]any) {
			out = append(out, v.(string))
		}
		return out
	}
	for name, c := range map[string]struct{ schema, code []string }{
		"kinds":      {enum("subscription", "properties", "kinds", "items"), kinds},
		"handlers":   {enum("subscription", "properties", "handler"), handlers},
		"roles":      {enum("recipient", "properties", "role"), roles},
		"conditions": {enum("recipient", "properties", "when", "items"), conditions},
	} {
		if !slices.Equal(c.schema, c.code) {
			t.Errorf("%s: the schema names %v, the code %v", name, c.schema, c.code)
		}
	}
}

// A file that names something this release does not know, or is not well
// formed, is refused by Parse and by the schema alike.
func TestParseRefuses(t *testing.T) {
	schema := schemaDoc(t)
	sub := `{"name":"x","kinds":["board.changed"],"to":[{"role":"coordinator","when":["always"]}],"handler":"wake"}`
	for name, raw := range map[string]string{
		"an unknown kind":      strings.Replace(sub, "board.changed", "board.moved", 1),
		"an unknown role":      strings.Replace(sub, `"coordinator"`, `"architect"`, 1),
		"an unknown condition": strings.Replace(sub, `"always"`, `"sometimes"`, 1),
		"an unknown handler":   strings.Replace(sub, `"wake"`, `"page"`, 1),
		"an unknown field":     strings.Replace(sub, `"handler"`, `"priority":1,"handler"`, 1),
		"a bad name":           strings.Replace(sub, `"x"`, `"X Y"`, 1),
		"no condition":         strings.Replace(sub, `["always"]`, `[]`, 1),
		"a repeated name":      sub + `,` + sub,
	} {
		file := `{"version":1,"subscriptions":[` + raw + `]}`
		if _, err := Parse([]byte(file)); err == nil {
			t.Errorf("%s: Parse took it", name)
		}
		var doc any
		json.Unmarshal([]byte(file), &doc)
		if name != "a repeated name" && len(validate(schema, schema, doc, "file")) == 0 {
			t.Errorf("%s: the schema took it", name)
		}
	}
	if _, err := Parse([]byte(`{"version":2,"subscriptions":[` + sub + `]}`)); err == nil {
		t.Error("another version was taken")
	}
	if s, err := Parse([]byte(`{"version":1,"subscriptions":[` + sub + `]}`)); err != nil || s.Version == Defaults().Version {
		t.Errorf("a valid file: %+v, %v", s, err)
	}
}

// A recipient is woken when every condition it names holds; a role named
// twice is woken once.
func TestRoles(t *testing.T) {
	d := Defaults()
	board, _ := d.Named("board-wake")
	forge, _ := d.Named("forge-wake")
	for _, c := range []struct {
		sub  Subscription
		f    Facts
		want []string
	}{
		{board, Facts{Ready: true}, []string{RoleCoordinator}},
		{board, Facts{Ready: true, OwnChange: true}, nil},
		{board, Facts{TeamAttempt: true}, []string{RoleCoordinator}},
		{board, Facts{}, nil},
		{forge, Facts{}, []string{RoleWorker}},
		{forge, Facts{Submit: true}, []string{RoleWorker, RoleCoordinator}},
		{forge, Facts{Submitted: true}, []string{RoleWorker, RoleCoordinator}},
		{Subscription{To: []Recipient{{RoleWorker, []string{WhenAlways}}, {RoleWorker, []string{WhenAlways}}}}, Facts{}, []string{RoleWorker}},
	} {
		if got := c.sub.Roles(c.f); !slices.Equal(got, c.want) {
			t.Errorf("%s with %+v: %v, want %v", c.sub.Name, c.f, got, c.want)
		}
	}
}
