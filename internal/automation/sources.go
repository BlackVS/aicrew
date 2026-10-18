package automation

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/BlackVS/aicrew/internal/events"
)

// The automations' entries and their precedence (docs/DESIGN-TURNS.md, 9.2,
// 13.1 and 13.3). An entry names an automation, where its script comes from
// and when it runs. The release's defaults and a process manifest's
// `automations` hold entries of one format, which automations.schema.json
// publishes; a script's first line is its header, which header.schema.json
// publishes. Both schemas are versioned with the host API.

// EntriesSchemaJSON is the published JSON Schema of the automations
// entries.
//
//go:embed automations.schema.json
var EntriesSchemaJSON []byte

// HeaderSchemaJSON is the published JSON Schema of a script's header.
//
//go:embed header.schema.json
var HeaderSchemaJSON []byte

// The extension points of 9.1, in the state machine's order.
const (
	PointAfterPush    = "after-push"    // a new head, before the self-review
	PointAfterCI      = "after-ci"      // CI concluded at the head
	PointCIFailed     = "ci-failed"     // the change failed CI
	PointGatesGreen   = "gates-green"   // the reviews and CI are green at the head
	PointBeforeSubmit = "before-submit" // the last point before the harness submits
	PointAfterMerge   = "after-merge"   // before the delivery is recorded
)

// Points is the extension points, in order.
func Points() []string { return slices.Clone(points) }

var points = []string{PointAfterPush, PointAfterCI, PointCIFailed, PointGatesGreen, PointBeforeSubmit, PointAfterMerge}

// Where an entry's script comes from.
const (
	// SourceRelease is a default the release embeds, under
	// defaults/automations/.
	SourceRelease = "release"
	// SourceProcess is a script beside the manifest, at the pinned commit.
	SourceProcess = "process"
	// SourceRepository is a script in the repository's automations/, at
	// the base branch's merged commit, never the attempt's head.
	SourceRepository = "repository"
	// SourceDisabled turns the release default of the same name off.
	SourceDisabled = "disabled"
	// SourceCommand is a declared command (9.1): an argument list the
	// launcher runs at a point, with no shell, in the attempt's worktree,
	// under the turn's allow list. It has no script.
	SourceCommand = "command"
)

// The bounds of the entries.
const (
	// MaxEntries bounds the entries of one file.
	MaxEntries = 64
	// MaxManifest bounds a manifest or a defaults file.
	MaxManifest = 256 << 10
	// MaxSource bounds a script.
	MaxSource = maxSource
	// maxDescription bounds an entry's or a header's description, in
	// characters.
	maxDescription = 1000
	// MaxCommandArgs bounds a declared command's argument list, and
	// maxCommandArg each argument, in bytes.
	MaxCommandArgs = 32
	maxCommandArg  = 256
)

// The shapes the schemas name; TestSchemasNameTheCodesShapes keeps them
// equal.
const (
	namePattern       = `^[a-z][a-z0-9-]{0,62}$`
	scriptPattern     = `^([A-Za-z0-9_-][A-Za-z0-9._-]*/)*[A-Za-z0-9_-][A-Za-z0-9._-]*\.star$`
	repositoryPattern = `^automations/([A-Za-z0-9_-][A-Za-z0-9._-]*/)*[A-Za-z0-9_-][A-Za-z0-9._-]*\.star$`
	turnPattern       = `^[a-z0-9_]{1,64}$`
	maxScriptPath     = 256
)

var (
	nameShape       = regexp.MustCompile(namePattern)
	scriptShape     = regexp.MustCompile(scriptPattern)
	repositoryShape = regexp.MustCompile(repositoryPattern)
	turnShape       = regexp.MustCompile(turnPattern)
)

// Entry is one automations entry. Script is the script's path: for the
// process, from the process repository's root, as the manifest names its
// other files, and in the manifest's directory (the launcher checks that,
// since it knows the manifest's path); for the repository, from its root
// and under automations/; for the release, under defaults/automations/. A
// disabled entry has no script, point or events. A command entry has
// Command in place of Script, and a point alone: it is run by the launcher
// at that point, never for an event.
type Entry struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Source      string   `json:"source"`
	Script      string   `json:"script,omitempty"`
	Command     []string `json:"command,omitempty"`
	Point       string   `json:"point,omitempty"`
	Events      []string `json:"events,omitempty"`
	Turn        string   `json:"turn,omitempty"`
}

// entryJSON is an entry as a file holds it: the source is "disabled" or an
// object naming the source and the script's path.
type entryJSON struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Source      json.RawMessage `json:"source"`
	Point       string          `json:"point"`
	Events      []string        `json:"events"`
	Turn        string          `json:"turn"`
}

// ParseManifest reads the automations entries of a process manifest. A
// manifest without `automations` has none; its other fields are not read.
func ParseManifest(raw []byte) ([]Entry, error) {
	if len(raw) > MaxManifest {
		return nil, fmt.Errorf("the manifest is %d bytes, more than %d", len(raw), MaxManifest)
	}
	var top struct {
		Automations json.RawMessage `json:"automations"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("the manifest is not a JSON object: %w", err)
	}
	if len(top.Automations) == 0 || string(top.Automations) == "null" {
		return nil, nil
	}
	es, err := parseEntries(top.Automations, false)
	if err != nil {
		return nil, fmt.Errorf("the manifest's automations: %w", err)
	}
	return es, nil
}

// ParseDefaults reads the release's defaults file: version 1, and entries
// whose source is the release.
func ParseDefaults(raw []byte) ([]Entry, error) {
	if len(raw) > MaxManifest {
		return nil, fmt.Errorf("the defaults file is %d bytes, more than %d", len(raw), MaxManifest)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f struct {
		Schema      string          `json:"$schema"`
		Version     int             `json:"version"`
		Automations json.RawMessage `json:"automations"`
	}
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("the defaults file is not valid: %w", err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("the defaults file's version is %d, this release reads 1", f.Version)
	}
	if len(f.Automations) == 0 {
		return nil, errors.New("the defaults file has no automations list")
	}
	return parseEntries(f.Automations, true)
}

func parseEntries(raw json.RawMessage, release bool) ([]Entry, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New("automations is not a list")
	}
	if len(items) > MaxEntries {
		return nil, fmt.Errorf("%d entries, more than %d", len(items), MaxEntries)
	}
	out := make([]Entry, 0, len(items))
	seen := map[string]bool{}
	for i, item := range items {
		e, err := parseEntry(item, release)
		if err != nil {
			if e.Name != "" {
				return nil, fmt.Errorf("entry %s: %w", e.Name, err)
			}
			return nil, fmt.Errorf("entry %d: %w", i+1, err)
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("entry %s: the name is used twice", e.Name)
		}
		seen[e.Name] = true
		out = append(out, e)
	}
	return out, nil
}

func parseEntry(raw json.RawMessage, release bool) (Entry, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var j entryJSON
	if err := dec.Decode(&j); err != nil {
		return Entry{}, fmt.Errorf("not a valid entry: %w", err)
	}
	e := Entry{Name: j.Name, Description: j.Description, Point: j.Point, Events: j.Events, Turn: j.Turn}
	if !nameShape.MatchString(e.Name) {
		return Entry{}, fmt.Errorf("the name %q is not lowercase letters, digits and '-', starting with a letter", e.Name)
	}
	if utf8.RuneCountInString(e.Description) > maxDescription {
		return e, fmt.Errorf("the description is longer than %d characters", maxDescription)
	}
	var disabled string
	if json.Unmarshal(j.Source, &disabled) == nil {
		if disabled != SourceDisabled {
			return e, fmt.Errorf("the source %q is not \"disabled\" or an object naming the script", disabled)
		}
		if e.Point != "" || len(e.Events) != 0 || e.Turn != "" {
			return e, errors.New("a disabled entry names no point, events or turn")
		}
		e.Source = SourceDisabled
		return e, nil
	}
	var src map[string]json.RawMessage
	if err := json.Unmarshal(j.Source, &src); err != nil || len(src) != 1 {
		return e, errors.New("the source is not \"disabled\" or an object naming one source and the script's path")
	}
	for k, v := range src {
		e.Source = k
		if k == SourceCommand {
			if err := json.Unmarshal(v, &e.Command); err != nil {
				return e, errors.New("the command is not a list of strings")
			}
		} else if err := json.Unmarshal(v, &e.Script); err != nil {
			return e, errors.New("the script's path is not a string")
		}
	}
	switch {
	case e.Source == SourceCommand && release:
		return e, errors.New("a release default's source is the release, not a command")
	case e.Source == SourceCommand:
		if err := checkCommand(e.Command); err != nil {
			return e, err
		}
		if e.Point == "" || len(e.Events) != 0 || e.Turn != "" {
			return e, errors.New("a command runs at a point: it names a point, and no events or turn")
		}
	case e.Source == SourceRelease && !release:
		return e, errors.New("the release source is the release's own; a manifest names process or repository")
	case e.Source != SourceRelease && release:
		return e, fmt.Errorf("a release default's source is the release, not %q", e.Source)
	case e.Source == SourceRepository:
		if len(e.Script) > maxScriptPath || !repositoryShape.MatchString(e.Script) {
			return e, fmt.Errorf("the repository script %q is not a .star path under automations/", e.Script)
		}
	case e.Source == SourceProcess || e.Source == SourceRelease:
		if len(e.Script) > maxScriptPath || !scriptShape.MatchString(e.Script) {
			return e, fmt.Errorf("the %s script %q is not a relative .star path", e.Source, e.Script)
		}
	default:
		return e, fmt.Errorf("unknown source %q", e.Source)
	}
	if e.Point == "" && len(e.Events) == 0 {
		return e, errors.New("it names no point and no events: nothing would run it")
	}
	if e.Point != "" && !slices.Contains(points, e.Point) {
		return e, fmt.Errorf("unknown point %q", e.Point)
	}
	if e.Events != nil && len(e.Events) == 0 {
		return e, errors.New("events is empty")
	}
	kinds := events.Kinds()
	for i, k := range e.Events {
		if !slices.Contains(kinds, k) {
			return e, fmt.Errorf("unknown event kind %q", k)
		}
		if slices.Contains(e.Events[:i], k) {
			return e, fmt.Errorf("the event kind %q is named twice", k)
		}
	}
	if e.Turn != "" && !turnShape.MatchString(e.Turn) {
		return e, fmt.Errorf("the turn kind %q is not lowercase letters, digits and '_'", e.Turn)
	}
	return e, nil
}

// checkCommand is the shape of a declared command: one to MaxCommandArgs
// arguments, each one to maxCommandArg bytes of UTF-8 without control
// characters. The first is the program, which the launcher checks against
// the turn's allow list when it runs the command.
func checkCommand(cmd []string) error {
	if len(cmd) == 0 || len(cmd) > MaxCommandArgs {
		return fmt.Errorf("a command is a list of 1 to %d strings, the program first", MaxCommandArgs)
	}
	for i, a := range cmd {
		if a == "" || len(a) > maxCommandArg || !utf8.ValidString(a) || strings.ContainsAny(a, "\x00\n\r\t") ||
			strings.IndexFunc(a, unicode.IsControl) >= 0 {
			return fmt.Errorf("command argument %d is empty, longer than %d bytes, or holds a control character", i+1, maxCommandArg)
		}
	}
	return nil
}

// Resolve applies the precedence of 13.1 by name. A process entry wins over
// the release default of the same name: it replaces it, or disables it. A
// process name the defaults do not have is added. A default the process
// does not name applies. The result keeps the defaults' order, with the
// process's new names after them in the manifest's order; a disabled entry
// stays in it, so the effective set shows what was turned off.
func Resolve(defaults, process []Entry) []Entry {
	byName := map[string]Entry{}
	for _, e := range process {
		byName[e.Name] = e
	}
	out := make([]Entry, 0, len(defaults)+len(process))
	have := map[string]bool{}
	for _, d := range defaults {
		if p, ok := byName[d.Name]; ok {
			out = append(out, p)
		} else {
			out = append(out, d)
		}
		have[d.Name] = true
	}
	for _, p := range process {
		if !have[p.Name] {
			out = append(out, p)
		}
	}
	return out
}

// HeaderPrefix starts a script's first line; the header's JSON object
// follows it on that line.
const HeaderPrefix = "# aicrew-automation: "

// Header is a script's header.
type Header struct {
	// HostAPI is the version of the host API the script was written for.
	HostAPI     int    `json:"host_api"`
	Description string `json:"description,omitempty"`
}

// ParseHeader reads a script's header from its first line. A script without
// one, or written for a host API newer than this release's, is refused.
func ParseHeader(src []byte) (Header, error) {
	line, _, _ := bytes.Cut(src, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	rest, ok := bytes.CutPrefix(line, []byte(HeaderPrefix))
	if !ok {
		return Header{}, fmt.Errorf("%w: its first line is not the header %q followed by a JSON object", ErrInvalid, strings.TrimSpace(HeaderPrefix))
	}
	dec := json.NewDecoder(bytes.NewReader(rest))
	dec.DisallowUnknownFields()
	var h struct {
		HostAPI     *int   `json:"host_api"`
		Description string `json:"description"`
	}
	if err := dec.Decode(&h); err != nil || dec.More() {
		return Header{}, fmt.Errorf("%w: the header is not one JSON object of host_api and description", ErrInvalid)
	}
	switch {
	case h.HostAPI == nil:
		return Header{}, fmt.Errorf("%w: the header names no host_api", ErrInvalid)
	case *h.HostAPI < 1:
		return Header{}, fmt.Errorf("%w: the header's host_api is %d", ErrInvalid, *h.HostAPI)
	case *h.HostAPI > HostAPIVersion:
		return Header{}, fmt.Errorf("%w: the script is written for host API %d; this release serves %d", ErrInvalid, *h.HostAPI, HostAPIVersion)
	case utf8.RuneCountInString(h.Description) > maxDescription:
		return Header{}, fmt.Errorf("%w: the header's description is longer than %d characters", ErrInvalid, maxDescription)
	}
	return Header{HostAPI: *h.HostAPI, Description: h.Description}, nil
}
