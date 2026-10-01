package agent

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

// The supported set (decision K2): the versions of aimem, ai-skills and the
// clients this aicrew-agent build is reviewed against, and the skills an
// agent home needs. It is embedded, so it changes only through a reviewed
// change to this repository.

//go:embed supported.json
var supportedJSON []byte

// Component states of a detected version against the set.
const (
	StateSupported = "supported" // within a supported range
	StateNewer     = "newer"     // above what was tested: works, with a notice
	StateUnknown   = "unknown"   // no release version could be read: neither blocks nor counts as supported
	StateBelow     = "below"     // older than the minimum: blocked
	StateMissing   = "missing"   // not installed: blocked
	StateFailed    = "failed"    // installed but could not be run or read: blocked
)

// versionRange is one supported line of a component: from Min, tested up to
// Tested (empty when no release of the line has been tested yet).
type versionRange struct {
	Min    string `json:"min"`
	Tested string `json:"tested,omitempty"`
}

type supportedComponent struct {
	Ranges []versionRange `json:"ranges"`
	Note   string         `json:"note,omitempty"`
}

// SupportedSet is the embedded set.
type SupportedSet struct {
	Version        int                           `json:"version"`
	RequiredSkills []string                      `json:"required_skills"`
	Components     map[string]supportedComponent `json:"components"`
}

// The components the set must name.
var setComponents = []string{"aimem", "ai-skills", "claude", "opencode"}

// LoadSupported decodes and checks the embedded set.
func LoadSupported() (SupportedSet, error) { return parseSupported(supportedJSON) }

func parseSupported(raw []byte) (SupportedSet, error) {
	var s SupportedSet
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return SupportedSet{}, fmt.Errorf("supported set: %w", err)
	}
	if s.Version != 1 {
		return SupportedSet{}, fmt.Errorf("supported set: version %d is not 1", s.Version)
	}
	for _, name := range setComponents {
		c, ok := s.Components[name]
		if !ok || len(c.Ranges) == 0 {
			return SupportedSet{}, fmt.Errorf("supported set: %s has no range", name)
		}
		for _, r := range c.Ranges {
			if _, dev, ok := parseVersion(r.Min); !ok || dev {
				return SupportedSet{}, fmt.Errorf("supported set: %s min %q is not a release version", name, r.Min)
			}
			if r.Tested == "" {
				continue
			}
			t, dev, ok := parseVersion(r.Tested)
			m, _, _ := parseVersion(r.Min)
			if !ok || dev || t.less(m) || t[0] != m[0] {
				return SupportedSet{}, fmt.Errorf("supported set: %s tested %q is not a release of the %s line", name, r.Tested, r.Min)
			}
		}
	}
	for _, sk := range s.RequiredSkills {
		if !skillName.MatchString(sk) {
			return SupportedSet{}, fmt.Errorf("supported set: %q is not a skill name", sk)
		}
	}
	return s, nil
}

var skillName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// semver is a release version's major, minor and patch.
type semver [3]int

func (v semver) less(w semver) bool {
	for i := range v {
		if v[i] != w[i] {
			return v[i] < w[i]
		}
	}
	return false
}

func (v semver) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

var versionText = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)([-+][0-9A-Za-z.-]+)?`)

// parseVersion reads the first X.Y.Z in s, with an optional leading v. dev
// reports a suffix: a prerelease, or a source build's `git describe`
// (v0.7.3-52-gabc), which is not a release that can be checked against the
// set.
func parseVersion(s string) (v semver, dev bool, ok bool) {
	m := versionText.FindStringSubmatch(s)
	if m == nil {
		return semver{}, false, false
	}
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return semver{}, false, false
		}
		v[i] = n
	}
	return v, m[4] != "", true
}

// classify places a release version against a component's ranges. A range
// covers its major line: below the line's minimum is below; above its tested
// release is newer. A major with no range is newer when it is above every
// line, and below otherwise.
func (c supportedComponent) classify(v semver) (state string, r versionRange) {
	var best *versionRange
	var bestMin semver
	top := semver{}
	for i := range c.Ranges {
		rg := &c.Ranges[i]
		m, _, _ := parseVersion(rg.Min)
		ceil := m
		if rg.Tested != "" {
			ceil, _, _ = parseVersion(rg.Tested)
		}
		if top.less(ceil) {
			top = ceil
		}
		if m[0] != v[0] || v.less(m) {
			continue
		}
		if best == nil || bestMin.less(m) {
			best, bestMin = rg, m
		}
	}
	if best == nil {
		for i := range c.Ranges {
			m, _, _ := parseVersion(c.Ranges[i].Min)
			if m[0] == v[0] {
				return StateBelow, c.Ranges[i] // same line, below its minimum
			}
		}
		if top.less(v) {
			return StateNewer, c.Ranges[len(c.Ranges)-1]
		}
		return StateBelow, c.Ranges[0]
	}
	if best.Tested != "" {
		if t, _, _ := parseVersion(best.Tested); t.less(v) {
			return StateNewer, *best
		}
	}
	return StateSupported, *best
}

// minimum is the lowest supported version of a component, for messages.
func (c supportedComponent) minimum() string { return c.Ranges[0].Min }
