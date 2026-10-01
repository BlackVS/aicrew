package version

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestLineAndOverride(t *testing.T) {
	for _, c := range []struct {
		i    Info
		want string
	}{
		{Info{Version: "v1.2.3"}, "aicrew v1.2.3"},
		{Info{Version: "dev", Commit: "0123456789abcdef0123"}, "aicrew dev (0123456789ab)"},
		{Info{Version: "dev", Commit: "abc", Modified: true}, "aicrew dev (abc, modified)"},
	} {
		if got := c.i.Line("aicrew"); got != c.want {
			t.Fatalf("%+v: %q, want %q", c.i, got, c.want)
		}
	}
	defer func(o string) { Override = o }(Override)
	Override = "v9.8.7"
	var out bytes.Buffer
	Print(&out, "aicrewd", true)
	var i Info
	if err := json.Unmarshal(out.Bytes(), &i); err != nil || i.Version != "v9.8.7" || i.Go == "" {
		t.Fatalf("%s: %v", out.String(), err)
	}
}
