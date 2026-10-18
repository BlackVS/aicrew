package agent

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
)

// The names an observation carries (increment 2d-d): every entry of the
// effective set but a disabled one, so a process that disables gates-green
// leaves it out and aicrewd keeps the gates wake; the release's defaults
// alone without a pin; nothing when the manifest cannot be read, which also
// keeps the wake. The names are kept per pin: a second read runs no git.
func TestAutomationNames(t *testing.T) {
	a := setupAutomationSources(t)
	srv := &StepServer{home: t.TempDir(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	src := a.sources(a.pin)
	names := srv.AutomationNames(t.Context(), src)
	if !slices.Equal(names, []string{"base-update", "ci-rerun", "project-check"}) {
		t.Fatalf("the names with the pin: %v", names)
	}
	calls := 0
	oldGit := automationGit
	automationGit = func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		calls++
		return oldGit(ctx, dir, env, args...)
	}
	t.Cleanup(func() { automationGit = oldGit })
	if again := srv.AutomationNames(t.Context(), src); !slices.Equal(again, names) || calls != 0 {
		t.Errorf("the second read: %v after %d git calls", again, calls)
	}
	if rel := srv.AutomationNames(t.Context(), AutomationSources{}); !slices.Equal(rel, []string{"base-update", "gates-green", "ci-rerun"}) {
		t.Errorf("the names without a pin: %v", rel)
	}
	bad := a.sources("0123456789abcdef0123456789abcdef01234567")
	if none := srv.AutomationNames(t.Context(), bad); none != nil {
		t.Errorf("the names of a pin that cannot be read: %v", none)
	}
}
