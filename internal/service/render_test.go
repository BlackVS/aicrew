package service

import (
	"encoding/xml"
	"strings"
	"testing"
	"unicode/utf16"
)

// awkward is a spec whose paths carry what each format must escape.
func awkward() Spec {
	home := `/home/m e/a%b$c"d\e&<f>`
	return Spec{Name: "aicrew-member-sol-0a1b2c3d", Label: "sol & co", Home: home,
		Exe:  `/opt/ai crew/aicrew-agent`,
		Args: []string{"serve", "--home", home, "--log", home + "/logs/serve.log"}, Path: "/usr/bin:/home/m e/.local/bin"}
}

// The unit runs the serve with its arguments quoted for systemd, restarts
// it after a failure but not after a refusal (exit 2), and starts at login.
func TestSystemdUnit(t *testing.T) {
	u := SystemdUnit(awkward())
	for _, want := range []string{
		"Description=aicrew member sol & co (/home/m e/a%%b$c\"d\\e&<f>)\n",
		`Environment="PATH=/usr/bin:/home/m e/.local/bin"` + "\n",
		`ExecStart="/opt/ai crew/aicrew-agent" "serve" "--home" "/home/m e/a%%b$$c\"d\\e&<f>" "--log" ` +
			`"/home/m e/a%%b$$c\"d\\e&<f>/logs/serve.log"` + "\n",
		"Restart=on-failure\n", "RestartPreventExitStatus=2\n", "UMask=0077\n", "WantedBy=default.target\n",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("the unit lacks %q:\n%s", want, u)
		}
	}
	if s := awkward(); true {
		s.Path = ""
		if strings.Contains(SystemdUnit(s), "Environment=") {
			t.Error("an empty PATH was written")
		}
	}
}

// The plist is well-formed XML whose program arguments are the spec's,
// escaped, and it keeps the agent alive after a failure only.
func TestLaunchdPlist(t *testing.T) {
	s := awkward()
	p := LaunchdPlist(s)
	var doc struct {
		Dict struct {
			Inner []byte `xml:",innerxml"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal([]byte(p), &doc); err != nil {
		t.Fatalf("the plist is not XML: %v\n%s", err, p)
	}
	dec := xml.NewDecoder(strings.NewReader(p))
	var strs []string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "string" {
			var v string
			if err := dec.DecodeElement(&v, &se); err != nil {
				t.Fatal(err)
			}
			strs = append(strs, v)
		}
	}
	want := append([]string{"io.github.blackvs.aicrew." + s.Name, s.Exe}, s.Args...)
	want = append(want, s.Path, "Background")
	if strings.Join(strs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the plist's strings:\n%q\nwant\n%q", strs, want)
	}
	for _, w := range []string{"<key>RunAtLoad</key>\n  <true/>", "<key>SuccessfulExit</key>\n    <false/>",
		"<key>Umask</key>\n  <integer>63</integer>"} {
		if !strings.Contains(p, w) {
			t.Errorf("the plist lacks %q", w)
		}
	}
}

// The task is UTF-16 XML that starts the serve at the member's logon,
// windowless, once at a time, without a time limit.
func TestTaskXML(t *testing.T) {
	s := Spec{Name: "aicrew-member-sol-0a1b2c3d", Label: "sol", Home: `C:\Users\m e\aicrew\agents\sol`,
		Exe:  `C:\Users\m e\AppData\Local\aicrew\bin\aicrew-agent.exe`,
		Args: []string{"serve", "--home", `C:\Users\m e\aicrew\agents\sol`, "--log", `C:\Users\m e\aicrew\agents\sol\logs\serve.log`}}
	raw := utf16File(TaskXML(s, `HOST\m e`))
	if raw[0] != 0xFF || raw[1] != 0xFE {
		t.Fatal("no UTF-16LE byte-order mark")
	}
	u := make([]uint16, 0, len(raw)/2)
	for i := 2; i+1 < len(raw); i += 2 {
		u = append(u, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	text := string(utf16.Decode(u))
	var task struct {
		Triggers struct {
			Logon struct {
				User string `xml:"UserId"`
			} `xml:"LogonTrigger"`
		}
		Principals struct {
			P struct {
				User      string `xml:"UserId"`
				LogonType string
				RunLevel  string
			} `xml:"Principal"`
		}
		Settings struct {
			MultipleInstancesPolicy string
			ExecutionTimeLimit      string
		}
		Actions struct {
			Exec struct {
				Command   string
				Arguments string
			}
		}
	}
	// encoding/xml refuses a declared UTF-16 encoding without a reader for
	// it; the text is already decoded.
	if err := xml.Unmarshal([]byte(strings.Replace(text, `encoding="UTF-16"`, "", 1)), &task); err != nil {
		t.Fatalf("the task is not XML: %v\n%s", err, text)
	}
	if task.Triggers.Logon.User != `HOST\m e` || task.Principals.P.User != `HOST\m e` ||
		task.Principals.P.LogonType != "InteractiveToken" || task.Principals.P.RunLevel != "LeastPrivilege" {
		t.Errorf("the trigger or principal: %+v %+v", task.Triggers, task.Principals)
	}
	if task.Settings.MultipleInstancesPolicy != "IgnoreNew" || task.Settings.ExecutionTimeLimit != "PT0S" {
		t.Errorf("the settings: %+v", task.Settings)
	}
	want := `--headless "C:\Users\m e\AppData\Local\aicrew\bin\aicrew-agent.exe" serve --home "C:\Users\m e\aicrew\agents\sol" ` +
		`--log "C:\Users\m e\aicrew\agents\sol\logs\serve.log"`
	if task.Actions.Exec.Command != "conhost.exe" || task.Actions.Exec.Arguments != want {
		t.Errorf("the action: %q %q, want conhost.exe %q", task.Actions.Exec.Command, task.Actions.Exec.Arguments, want)
	}
}

// Quoting leaves a plain word alone and quotes the rest as the C runtime
// reads it back; the Windows-only test checks the reading.
func TestWinQuote(t *testing.T) {
	for in, want := range map[string]string{
		"serve":       "serve",
		`C:\a b\c`:    `"C:\a b\c"`,
		`C:\a b\`:     `"C:\a b\\"`,
		`a"b`:         `"a\"b"`,
		`a\"b`:        `"a\\\"b"`,
		"":            `""`,
		`C:\plain\x\`: `C:\plain\x\`,
	} {
		if got := winQuote(in); got != want {
			t.Errorf("winQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// A name is stable for a home, differs between homes, and keeps only what
// every manager accepts.
func TestName(t *testing.T) {
	a, b := Name("/h/one", "Sol"), Name("/h/two", "Sol")
	if a == b || a != Name("/h/one/", "Sol") {
		t.Fatalf("names: %q %q", a, b)
	}
	if !strings.HasPrefix(a, "aicrew-member-sol-") || len(a) != len("aicrew-member-sol-")+8 {
		t.Fatalf("name %q", a)
	}
	for label, want := range map[string]string{"Opus 5.5 / worker": "opus-5-5-worker", "": "home", "--": "home",
		"ÄÖ": "home", strings.Repeat("x", 40): strings.Repeat("x", 32)} {
		if got := slug(label); got != want {
			t.Errorf("slug(%q) = %q, want %q", label, got, want)
		}
	}
}
