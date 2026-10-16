// Package schematest checks a JSON document against the subset of JSON
// Schema (draft 2020-12) that aicrew's published schemas use, so that a test
// can show a file the release reads matches the schema it publishes, without
// a schema library in the module: type (object, array, string, integer,
// boolean), const, enum, required, properties, additionalProperties false,
// items, minItems, maxItems, uniqueItems, pattern, maxLength, minimum,
// maximum, oneOf, anyOf and local $ref.
package schematest

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

// Validate checks doc, decoded with encoding/json into any, against schema,
// decoded the same way. It returns one line per violation, each starting
// with the path at, and nothing when doc matches.
func Validate(schema map[string]any, doc any, at string) []string {
	return validate(schema, schema, doc, at)
}

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
	if alts, ok := schema["oneOf"].([]any); ok {
		if n := matching(root, alts, doc, at); n != 1 {
			fail("matches %d of oneOf's %d alternatives, not exactly one", n, len(alts))
		}
	}
	if alts, ok := schema["anyOf"].([]any); ok {
		if matching(root, alts, doc, at) == 0 {
			fail("matches none of anyOf's %d alternatives", len(alts))
		}
	}
	switch schema["type"] {
	case "object":
		obj, ok := doc.(map[string]any)
		if !ok {
			fail("is not an object")
			return errs
		}
		props, _ := schema["properties"].(map[string]any)
		req, _ := schema["required"].([]any)
		for _, r := range req {
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
		if n, ok := schema["maxItems"].(float64); ok && len(arr) > int(n) {
			fail("has %d items, more than %v", len(arr), n)
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
	case "integer":
		n, ok := doc.(float64)
		if !ok || n != math.Trunc(n) {
			fail("is not an integer")
			return errs
		}
		if m, ok := schema["minimum"].(float64); ok && n < m {
			fail("is %v, less than %v", n, m)
		}
		if m, ok := schema["maximum"].(float64); ok && n > m {
			fail("is %v, more than %v", n, m)
		}
	case "boolean":
		if _, ok := doc.(bool); !ok {
			fail("is not a boolean")
		}
	}
	return errs
}

// matching is how many of the alternatives doc matches.
func matching(root map[string]any, alts []any, doc any, at string) int {
	n := 0
	for _, a := range alts {
		if len(validate(root, a.(map[string]any), doc, at)) == 0 {
			n++
		}
	}
	return n
}
