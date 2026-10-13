package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// withSelfOnPath puts this aicrew-agent's directory first on PATH and keeps
// the rest; it changes nothing when the directory leads already, makes an
// empty PATH the directory alone, keeps Windows' variable name, and leaves
// one PATH variable.
func TestWithSelfOnPath(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bin")
	prev := selfExecutable
	selfExecutable = func() string { return filepath.Join(bin, "aicrew-agent") }
	t.Cleanup(func() { selfExecutable = prev })
	sep := string(os.PathListSeparator)
	other := filepath.Join(t.TempDir(), "other")

	got := withSelfOnPath([]string{"HOME=/h", "PATH=" + other})
	if !slices.Equal(got, []string{"HOME=/h", "PATH=" + bin + sep + other}) {
		t.Errorf("an inherited PATH: %q", got)
	}
	led := []string{"PATH=" + bin + sep + other}
	if got := withSelfOnPath(led); !slices.Equal(got, led) {
		t.Errorf("a PATH led by the directory changed: %q", got)
	}
	if got := withSelfOnPath([]string{"HOME=/h"}); !slices.Equal(got, []string{"HOME=/h", "PATH=" + bin}) {
		t.Errorf("no PATH: %q", got)
	}
	if got := withSelfOnPath([]string{"PATH="}); !slices.Equal(got, []string{"PATH=" + bin}) {
		t.Errorf("an empty PATH: %q", got)
	}
	if runtime.GOOS == "windows" {
		got := withSelfOnPath([]string{"Path=" + other, "X=1"})
		if !slices.Equal(got, []string{"X=1", "Path=" + bin + sep + other}) {
			t.Errorf("Windows' Path: %q", got)
		}
	}
	selfExecutable = func() string { return "aicrew-agent" }
	if got := withSelfOnPath([]string{"PATH=" + other}); !slices.Equal(got, []string{"PATH=" + other}) {
		t.Errorf("an executable of unknown directory changed PATH: %q", got)
	}
}

// A check probe runs with this aicrew-agent first on PATH, as a launched
// client does.
func TestCheckProbeEnvPutsSelfOnPath(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bin")
	prev := selfExecutable
	selfExecutable = func() string { return filepath.Join(bin, "aicrew-agent") }
	t.Cleanup(func() { selfExecutable = prev })
	env := checkProbeEnv([]string{"PATH=/usr/bin", SessionEnv + "=/elsewhere"}, t.TempDir())
	n := 0
	for _, kv := range env {
		name, v, _ := strings.Cut(kv, "=")
		switch name {
		case "PATH":
			n++
			if !strings.HasPrefix(v, bin+string(os.PathListSeparator)) {
				t.Errorf("the probe's PATH is %q", v)
			}
		case SessionEnv:
			t.Errorf("the probe inherited %s", SessionEnv)
		}
	}
	if n != 1 {
		t.Errorf("%d PATH variables in %q", n, env)
	}
}

// withStopHookCap raises Claude Code's Stop-hook block cap to
// StopHookBlockCap, keeps a larger one, and leaves one variable.
func TestWithStopHookCap(t *testing.T) {
	want := StopHookBlockCapEnv + "=" + strconv.Itoa(StopHookBlockCap)
	if got := withStopHookCap([]string{"A=1"}); !slices.Equal(got, []string{"A=1", want}) {
		t.Errorf("no cap: %q", got)
	}
	for _, v := range []string{"9", "", "many", "-1"} {
		if got := withStopHookCap([]string{StopHookBlockCapEnv + "=" + v}); !slices.Equal(got, []string{want}) {
			t.Errorf("cap %q: %q", v, got)
		}
	}
	larger := []string{StopHookBlockCapEnv + "=" + strconv.Itoa(StopHookBlockCap*10)}
	if got := withStopHookCap(larger); !slices.Equal(got, larger) {
		t.Errorf("a larger cap changed: %q", got)
	}
}
