package events

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/schematest"
)

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
	if errs := schematest.Validate(schema, doc, "subscriptions.json"); len(errs) != 0 {
		t.Fatalf("the defaults do not match the schema:\n%s", strings.Join(errs, "\n"))
	}
	d := Defaults()
	if len(d.Subscriptions) != 4 || len(d.Version) != 16 || len(d.Matching(KindBoardChanged)) != 1 ||
		len(d.Matching(KindForgeChanged)) != 1 || len(d.Matching(KindAttemptChanged)) != 1 ||
		len(d.Matching(KindMemberChanged)) != 0 || len(d.Matching(KindTimerDue)) != 0 || len(d.Matching(KindAutomationRun)) != 1 {
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
		if name != "a repeated name" && len(schematest.Validate(schema, doc, "file")) == 0 {
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
	blocked, _ := d.Named("blocked-wake")
	auto, _ := d.Named("automation-wake")
	for _, c := range []struct {
		sub  Subscription
		f    Facts
		want []string
	}{
		{board, Facts{Ready: true}, []string{RoleCoordinator}},
		{board, Facts{Ready: true, OwnChange: true}, nil},
		{board, Facts{TeamAttempt: true}, []string{RoleCoordinator}},
		{board, Facts{}, nil},
		{forge, Facts{}, nil},
		{forge, Facts{Action: true}, []string{RoleWorker}},
		{forge, Facts{Submit: true, Action: true}, []string{RoleWorker, RoleCoordinator}},
		{forge, Facts{Submitted: true, Action: true}, []string{RoleWorker, RoleCoordinator}},
		{forge, Facts{Submitted: true}, nil},
		{blocked, Facts{Blocked: true}, []string{RoleCoordinator}},
		{blocked, Facts{}, nil},
		{auto, Facts{Failed: true}, []string{RoleCoordinator}},
		{auto, Facts{Turn: true}, []string{RoleWorker}},
		{auto, Facts{}, nil},
		{Subscription{To: []Recipient{{RoleWorker, []string{WhenAlways}}, {RoleWorker, []string{WhenAlways}}}}, Facts{}, []string{RoleWorker}},
	} {
		if got := c.sub.Roles(c.f); !slices.Equal(got, c.want) {
			t.Errorf("%s with %+v: %v, want %v", c.sub.Name, c.f, got, c.want)
		}
	}
}
