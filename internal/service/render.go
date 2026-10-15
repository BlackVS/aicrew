package service

import (
	"bytes"
	"encoding/xml"
	"strings"
	"unicode/utf16"
)

// The service definitions, rendered from a Spec. Each runs the serve under
// the member's own account and restarts it after a failure; none restarts
// a serve that refused to start (exit 2: a usage error, a home not in turns
// mode, or another serve of the home running).

// SystemdUnit is the systemd user unit.
func SystemdUnit(s Spec) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=aicrew member " + systemdText(s.Label) + " (" + systemdText(s.Home) + ")\n")
	b.WriteString("After=network-online.target\n\n")
	b.WriteString("[Service]\n")
	if s.Path != "" {
		b.WriteString("Environment=" + systemdQuote("PATH="+s.Path) + "\n")
	}
	words := []string{systemdQuote(s.Exe)}
	for _, a := range s.Args {
		words = append(words, systemdQuote(a))
	}
	b.WriteString("ExecStart=" + strings.Join(words, " ") + "\n")
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=10\n")
	b.WriteString("RestartPreventExitStatus=2\n")
	// serve stops a running turn and leaves the session on SIGTERM.
	b.WriteString("TimeoutStopSec=60\n")
	b.WriteString("UMask=0077\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// systemdQuote quotes one word of a unit's command line or assignment:
// within double quotes a backslash and a quote are escaped, and systemd's
// own specifiers (%) and variables ($) are doubled so they stay literal.
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$", "\n", " ")
	return `"` + r.Replace(s) + `"`
}

// systemdText keeps a free-text value on one line with its specifiers
// literal.
func systemdText(s string) string {
	return strings.NewReplacer("%", "%%", "\n", " ").Replace(s)
}

// launchdLabel is the launchd agent's label for a service name.
func launchdLabel(name string) string { return "io.github.blackvs.aicrew." + name }

// LaunchdPlist is the launchd agent's property list.
func LaunchdPlist(s Spec) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	key := func(k string) { b.WriteString("  <key>" + k + "</key>\n") }
	str := func(indent, v string) { b.WriteString(indent + "<string>" + xmlText(v) + "</string>\n") }
	key("Label")
	str("  ", launchdLabel(s.Name))
	key("ProgramArguments")
	b.WriteString("  <array>\n")
	str("    ", s.Exe)
	for _, a := range s.Args {
		str("    ", a)
	}
	b.WriteString("  </array>\n")
	if s.Path != "" {
		key("EnvironmentVariables")
		b.WriteString("  <dict>\n    <key>PATH</key>\n")
		str("    ", s.Path)
		b.WriteString("  </dict>\n")
	}
	key("RunAtLoad")
	b.WriteString("  <true/>\n")
	// Restart after a failure, not after a clean stop.
	key("KeepAlive")
	b.WriteString("  <dict>\n    <key>SuccessfulExit</key>\n    <false/>\n  </dict>\n")
	key("ThrottleInterval")
	b.WriteString("  <integer>10</integer>\n")
	key("ProcessType")
	b.WriteString("  <string>Background</string>\n")
	key("Umask")
	b.WriteString("  <integer>63</integer>\n") // 0077, as the unit's UMask
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// TaskXML is the Windows logon task, for schtasks /Create /XML. It starts
// the serve at the member's logon, windowless through conhost --headless,
// with the member's own token and no elevation, and never twice at once.
func TaskXML(s Spec, user string) string {
	words := []string{winQuote(s.Exe)}
	for _, a := range s.Args {
		words = append(words, winQuote(a))
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-16"?>` + "\n")
	b.WriteString(`<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` + "\n")
	b.WriteString("  <RegistrationInfo>\n")
	b.WriteString("    <Description>" + xmlText("aicrew member "+s.Label+" ("+s.Home+")") + "</Description>\n")
	b.WriteString("  </RegistrationInfo>\n")
	b.WriteString("  <Triggers>\n    <LogonTrigger>\n      <Enabled>true</Enabled>\n")
	b.WriteString("      <UserId>" + xmlText(user) + "</UserId>\n")
	b.WriteString("    </LogonTrigger>\n  </Triggers>\n")
	b.WriteString("  <Principals>\n    <Principal id=\"Author\">\n")
	b.WriteString("      <UserId>" + xmlText(user) + "</UserId>\n")
	b.WriteString("      <LogonType>InteractiveToken</LogonType>\n")
	b.WriteString("      <RunLevel>LeastPrivilege</RunLevel>\n")
	b.WriteString("    </Principal>\n  </Principals>\n")
	b.WriteString("  <Settings>\n")
	b.WriteString("    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>\n")
	b.WriteString("    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>\n")
	b.WriteString("    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>\n")
	b.WriteString("    <StartWhenAvailable>true</StartWhenAvailable>\n")
	b.WriteString("    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>\n")
	b.WriteString("    <RestartOnFailure>\n      <Interval>PT1M</Interval>\n      <Count>999</Count>\n    </RestartOnFailure>\n")
	b.WriteString("    <Enabled>true</Enabled>\n")
	b.WriteString("  </Settings>\n")
	b.WriteString("  <Actions Context=\"Author\">\n    <Exec>\n")
	b.WriteString("      <Command>conhost.exe</Command>\n")
	b.WriteString("      <Arguments>" + xmlText("--headless "+strings.Join(words, " ")) + "</Arguments>\n")
	b.WriteString("    </Exec>\n  </Actions>\n")
	b.WriteString("</Task>\n")
	return b.String()
}

// winQuote quotes one argument for a Windows command line as the C
// runtime and Go's os.Args read it back.
func winQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range s {
		switch r {
		case '\\':
			slashes++
		case '"':
			// The slashes already written are doubled, and one more
			// escapes the quote.
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
			b.WriteRune(r)
			continue
		default:
			slashes = 0
		}
		if r != '\\' {
			b.WriteRune(r)
		} else {
			b.WriteByte('\\')
		}
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	b.WriteByte('"')
	return b.String()
}

// utf16File encodes a task definition as schtasks reads it: UTF-16LE with
// a byte-order mark.
func utf16File(s string) []byte {
	u := utf16.Encode([]rune(s))
	out := make([]byte, 2, 2+2*len(u))
	out[0], out[1] = 0xFF, 0xFE
	for _, c := range u {
		out = append(out, byte(c), byte(c>>8))
	}
	return out
}
