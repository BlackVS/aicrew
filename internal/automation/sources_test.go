package automation

import (
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/defaults"
	"github.com/BlackVS/aicrew/internal/events"
	"github.com/BlackVS/aicrew/internal/schematest"
)

func schemaOf(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func decode(t *testing.T, raw string) any {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return doc
}

// A manifest's automations, each kind of entry once.
const manifestJSON = `{
  "name": "delivery",
  "roles": {"worker": "roles/worker.md"},
  "automations": [
    {"name": "base-update", "source": {"process": "automations/base-update.star"}, "point": "after-ci"},
    {"name": "gates-green", "source": "disabled", "description": "the team marks ready by hand"},
    {"name": "project-check", "source": {"repository": "automations/check.star"}, "point": "after-push",
     "events": ["forge.changed"], "turn": "rework"},
    {"name": "board-note", "source": {"process": "processes/delivery/note.star"}, "events": ["board.changed", "timer.due"]},
    {"name": "repo-check", "source": {"command": ["bash", "scripts/check-repo.sh"]}, "point": "after-push",
     "description": "the repository's own checks"}
  ]
}`

// The manifest's entries and the release's defaults file validate against
// the published schema, and the parsers read them.
func TestEntriesMatchTheSchema(t *testing.T) {
	schema := schemaOf(t, EntriesSchemaJSON)
	if errs := schematest.Validate(schema, decode(t, manifestJSON), "manifest"); len(errs) != 0 {
		t.Fatalf("the manifest does not match the schema:\n%s", strings.Join(errs, "\n"))
	}
	es, err := ParseManifest([]byte(manifestJSON))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{Name: "base-update", Source: SourceProcess, Script: "automations/base-update.star", Point: PointAfterCI},
		{Name: "gates-green", Source: SourceDisabled, Description: "the team marks ready by hand"},
		{Name: "project-check", Source: SourceRepository, Script: "automations/check.star", Point: PointAfterPush,
			Events: []string{"forge.changed"}, Turn: "rework"},
		{Name: "board-note", Source: SourceProcess, Script: "processes/delivery/note.star", Events: []string{"board.changed", "timer.due"}},
		{Name: "repo-check", Source: SourceCommand, Command: []string{"bash", "scripts/check-repo.sh"}, Point: PointAfterPush,
			Description: "the repository's own checks"},
	}
	if !reflect.DeepEqual(es, want) {
		t.Fatalf("parsed %+v", es)
	}

	raw, err := fs.ReadFile(defaults.Automations, defaults.AutomationsFile)
	if err != nil {
		t.Fatal(err)
	}
	if errs := schematest.Validate(schema, decode(t, string(raw)), "defaults"); len(errs) != 0 {
		t.Fatalf("the release's defaults do not match the schema:\n%s", strings.Join(errs, "\n"))
	}
	if d, err := ParseDefaults(raw); err != nil || len(d) != 4 {
		t.Fatalf("the release's defaults (review-request, ci-rerun and gates-green since 2d-d, base-update since 2e): %+v, %v", d, err)
	}
	// A manifest without automations has none, as an aiskills process
	// manifest of today's shape.
	today := `{"version": 1, "handbook": "processes/aimem/handbook.md",
		"checklists": {"READY": "processes/aimem/ready.json", "DONE": "processes/aimem/done.json"},
		"templates": {"task": "processes/aimem/task.json"}, "skills": ["oh-code-review"], "budget_bytes": 12288}`
	if es, err := ParseManifest([]byte(today)); err != nil || es != nil {
		t.Fatalf("a manifest without automations: %+v, %v", es, err)
	}
	// The manifest's own version is the manifest format's: a later one
	// still validates and parses.
	later := strings.Replace(manifestJSON, `"name": "delivery",`, `"version": 2, "name": "delivery",`, 1)
	if errs := schematest.Validate(schema, decode(t, later), "manifest"); len(errs) != 0 {
		t.Fatalf("a manifest of a later format version does not match the schema:\n%s", strings.Join(errs, "\n"))
	}
	if es, err := ParseManifest([]byte(later)); err != nil || len(es) != 5 {
		t.Fatalf("a manifest of a later format version: %+v, %v", es, err)
	}
}

// The schemas name the code's vocabulary and shapes: the points, the event
// kinds, the patterns and the host API's version.
func TestSchemasNameTheCodesShapes(t *testing.T) {
	defs := schemaOf(t, EntriesSchemaJSON)["$defs"].(map[string]any)
	script := defs["script"].(map[string]any)["properties"].(map[string]any)
	strs := func(v any) []string {
		var out []string
		for _, x := range v.([]any) {
			out = append(out, x.(string))
		}
		return out
	}
	if got := strs(script["point"].(map[string]any)["enum"]); !slices.Equal(got, points) {
		t.Errorf("the schema's points %v, the code's %v", got, points)
	}
	if got := strs(script["events"].(map[string]any)["items"].(map[string]any)["enum"]); !slices.Equal(got, events.Kinds()) {
		t.Errorf("the schema's event kinds %v, the code's %v", got, events.Kinds())
	}
	sources := script["source"].(map[string]any)["oneOf"].([]any)
	pattern := func(i int, key string) string {
		return sources[i].(map[string]any)["properties"].(map[string]any)[key].(map[string]any)["pattern"].(string)
	}
	for _, c := range []struct{ schema, code string }{
		{defs["name"].(map[string]any)["pattern"].(string), namePattern},
		{pattern(0, SourceProcess), scriptPattern},
		{pattern(1, SourceRepository), repositoryPattern},
		{pattern(2, SourceRelease), scriptPattern},
		{script["turn"].(map[string]any)["pattern"].(string), turnPattern},
	} {
		if c.schema != c.code {
			t.Errorf("the schema's pattern %s, the code's %s", c.schema, c.code)
		}
	}
	command := defs["command"].(map[string]any)["properties"].(map[string]any)
	if got := strs(command["point"].(map[string]any)["enum"]); !slices.Equal(got, points) {
		t.Errorf("the command schema's points %v, the code's %v", got, points)
	}
	args := command["source"].(map[string]any)["properties"].(map[string]any)["command"].(map[string]any)
	if args["maxItems"].(float64) != MaxCommandArgs || args["items"].(map[string]any)["maxLength"].(float64) != maxCommandArg {
		t.Errorf("the command schema's bounds %v, the code's %d and %d", args, MaxCommandArgs, maxCommandArg)
	}
	host := schemaOf(t, HeaderSchemaJSON)["properties"].(map[string]any)["host_api"].(map[string]any)
	if host["maximum"].(float64) != HostAPIVersion {
		t.Errorf("the header schema allows host API %v, the release serves %d", host["maximum"], HostAPIVersion)
	}
}

// An entry the release cannot run is refused by the parser and by the
// schema alike.
func TestParseManifestRefuses(t *testing.T) {
	schema := schemaOf(t, EntriesSchemaJSON)
	entry := `{"name":"x","source":{"process":"x.star"},"point":"after-ci"}`
	for name, raw := range map[string]string{
		"a bad name":                             strings.Replace(entry, `"x"`, `"X Y"`, 1),
		"an unknown point":                       strings.Replace(entry, `"after-ci"`, `"after-lunch"`, 1),
		"an unknown source":                      strings.Replace(entry, `"process"`, `"elsewhere"`, 1),
		"two sources":                            strings.Replace(entry, `{"process":"x.star"}`, `{"process":"x.star","repository":"automations/x.star"}`, 1),
		"a path out of the directory":            strings.Replace(entry, `"x.star"`, `"../x.star"`, 1),
		"an absolute path":                       strings.Replace(entry, `"x.star"`, `"/x.star"`, 1),
		"not a .star script":                     strings.Replace(entry, `"x.star"`, `"x.sh"`, 1),
		"a repository path outside automations/": `{"name":"x","source":{"repository":"scripts/x.star"},"point":"after-ci"}`,
		"neither point nor events":               `{"name":"x","source":{"process":"x.star"}}`,
		"an unknown event kind":                  `{"name":"x","source":{"process":"x.star"},"events":["board.moved"]}`,
		"no event kind":                          `{"name":"x","source":{"process":"x.star"},"events":[]}`,
		"a bad turn kind":                        strings.Replace(entry, `"point"`, `"turn":"Re-Work","point"`, 1),
		"an unknown field":                       strings.Replace(entry, `"point"`, `"priority":1,"point"`, 1),
		"a disabled entry with a point":          `{"name":"x","source":"disabled","point":"after-ci"}`,
		"a source that is another word":          `{"name":"x","source":"off"}`,
		"too many entries":                       strings.Repeat(entry+",", MaxEntries) + entry,
		// A command: an argument list at a point, nothing else.
		"a command with no arguments":   `{"name":"x","source":{"command":[]},"point":"after-push"}`,
		"a command that is a string":    `{"name":"x","source":{"command":"bash scripts/x.sh"},"point":"after-push"}`,
		"a command with an empty word":  `{"name":"x","source":{"command":["bash",""]},"point":"after-push"}`,
		"a command with a newline":      `{"name":"x","source":{"command":["bash","a\nb"]},"point":"after-push"}`,
		"a command without a point":     `{"name":"x","source":{"command":["bash","x.sh"]}}`,
		"a command at an event":         `{"name":"x","source":{"command":["bash","x.sh"]},"point":"after-push","events":["forge.changed"]}`,
		"a command with a turn":         `{"name":"x","source":{"command":["bash","x.sh"]},"point":"after-push","turn":"rework"}`,
		"a command with too many words": `{"name":"x","source":{"command":[` + strings.Repeat(`"a",`, MaxCommandArgs) + `"a"]},"point":"after-push"}`,
		"a command with a long word":    `{"name":"x","source":{"command":["bash","` + strings.Repeat("a", maxCommandArg+1) + `"]},"point":"after-push"}`,
		// The schema allows the release source, which is the defaults
		// file's; the parser refuses it in a manifest.
		"the release source": strings.Replace(entry, `"process"`, `"release"`, 1),
		// Unique names are the parser's.
		"a repeated name": entry + "," + entry,
	} {
		file := `{"automations":[` + raw + `]}`
		if _, err := ParseManifest([]byte(file)); err == nil {
			t.Errorf("%s: the parser took it", name)
		}
		if name != "the release source" && name != "a repeated name" &&
			len(schematest.Validate(schema, decode(t, file), "manifest")) == 0 {
			t.Errorf("%s: the schema took it", name)
		}
	}
	if _, err := ParseManifest([]byte(`[]`)); err == nil {
		t.Error("a manifest that is not an object was taken")
	}
	if _, err := ParseDefaults([]byte(`{"version":1,"automations":[` + entry + `]}`)); err == nil {
		t.Error("a release default whose source is the process was taken")
	}
	if _, err := ParseDefaults([]byte(`{"version":1,"automations":[{"name":"x","source":{"command":["bash","x.sh"]},"point":"after-push"}]}`)); err == nil {
		t.Error("a release default that is a command was taken")
	}
	if _, err := ParseDefaults([]byte(`{"version":2,"automations":[]}`)); err == nil {
		t.Error("another version of the defaults file was taken")
	}
}

// Precedence by name (13.1): the process replaces a default, disables it, or
// adds a name; a default the process does not name applies.
func TestResolvePrecedence(t *testing.T) {
	defaults := []Entry{
		{Name: "base-update", Source: SourceRelease, Script: "base-update.star", Point: PointAfterCI},
		{Name: "gates-green", Source: SourceRelease, Script: "gates-green.star", Point: PointGatesGreen},
		{Name: "ci-rerun", Source: SourceRelease, Script: "ci-rerun.star", Point: PointCIFailed},
	}
	process := []Entry{
		{Name: "project-check", Source: SourceRepository, Script: "automations/check.star", Point: PointAfterPush},
		{Name: "base-update", Source: SourceProcess, Script: "base-update.star", Events: []string{"forge.changed"}},
		{Name: "gates-green", Source: SourceDisabled},
	}
	got := Resolve(defaults, process)
	want := []Entry{process[1], process[2], defaults[2], process[0]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved %+v", got)
	}
	if got := Resolve(defaults, nil); !reflect.DeepEqual(got, defaults) {
		t.Errorf("without a process: %+v", got)
	}
}

// A script's header names the host API it was written for; a script
// without one, or written for a newer host API, is refused, and the schema
// agrees.
func TestParseHeader(t *testing.T) {
	schema := schemaOf(t, HeaderSchemaJSON)
	body := "\ndef main(event):\n    return None\n"
	h, err := ParseHeader([]byte(HeaderPrefix + `{"host_api": 1, "description": "update from main"}` + "\r" + body))
	if err != nil || h != (Header{HostAPI: 1, Description: "update from main"}) {
		t.Fatalf("a valid header: %+v, %v", h, err)
	}
	if errs := schematest.Validate(schema, decode(t, `{"host_api": 1, "description": "update from main"}`), "header"); len(errs) != 0 {
		t.Fatalf("the schema refused a valid header: %v", errs)
	}
	for name, src := range map[string]string{
		"no header":            "def main(event):\n    return None\n",
		"another comment":      "# base update\n" + body,
		"no host_api":          HeaderPrefix + `{"description": "x"}` + body,
		"a newer host API":     HeaderPrefix + `{"host_api": 2}` + body,
		"host API zero":        HeaderPrefix + `{"host_api": 0}` + body,
		"an unknown field":     HeaderPrefix + `{"host_api": 1, "points": []}` + body,
		"host_api as a string": HeaderPrefix + `{"host_api": "1"}` + body,
		"two objects":          HeaderPrefix + `{"host_api": 1} {"host_api": 1}` + body,
		"the header later":     body + HeaderPrefix + `{"host_api": 1}`,
	} {
		if _, err := ParseHeader([]byte(src)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
		line, _, _ := strings.Cut(src, "\n")
		obj, ok := strings.CutPrefix(line, HeaderPrefix)
		var doc any
		if ok && json.Unmarshal([]byte(obj), &doc) == nil && len(schematest.Validate(schema, doc, "header")) == 0 {
			t.Errorf("%s: the schema took it", name)
		}
	}
	// A script with a header runs as before: the header is a comment.
	if _, err := Run(t.Context(), Script{Name: "h.star", Source: []byte(HeaderPrefix + `{"host_api": 1}` + body)}, Event{}); err != nil {
		t.Fatalf("a script with a header: %v", err)
	}
}
