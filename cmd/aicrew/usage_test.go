package main

import (
	"bytes"
	"context"
	"slices"
	"sort"
	"strings"
	"testing"
)

// usageVerbs reads the verbs a usage text names for group: the third word
// of each "aicrew GROUP ..." line, split at "|", where it is a verb and not
// a flag or a placeholder.
func usageVerbs(text, group string) []string {
	var verbs []string
	for _, line := range strings.Split(text, "\n") {
		w := strings.Fields(line)
		if len(w) < 3 || w[0] != "aicrew" || w[1] != group {
			continue
		}
		if strings.HasPrefix(w[2], "-") || strings.HasPrefix(w[2], "[") || strings.ToUpper(w[2]) == w[2] {
			continue
		}
		for _, v := range strings.Split(w[2], "|") {
			if !slices.Contains(verbs, v) {
				verbs = append(verbs, v)
			}
		}
	}
	sort.Strings(verbs)
	return verbs
}

// The top-level usage names every verb of each command group, as the
// group's own usage lists the verbs it accepts: aicrew team's list once
// lacked toolchain, and aicrew escalations' lacked wait.
func TestUsageNamesEveryVerb(t *testing.T) {
	groups := map[string]bool{}
	for _, line := range strings.Split(usage, "\n") {
		if w := strings.Fields(line); len(w) >= 3 && w[0] == "aicrew" && len(usageVerbs(line, w[1])) > 0 {
			groups[w[1]] = true
		}
	}
	if len(groups) < 8 {
		t.Fatalf("the top-level usage names only the groups %v", groups)
	}
	for g := range groups {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{g}, &stdout, &stderr); code != 2 {
			t.Errorf("aicrew %s with no verb: exit %d, want 2", g, code)
		}
		own := usageVerbs(stderr.String(), g)
		if top := usageVerbs(usage, g); !slices.Equal(top, own) {
			t.Errorf("aicrew %s: the top-level usage names %v, the group's usage %v", g, top, own)
		}
	}
}
