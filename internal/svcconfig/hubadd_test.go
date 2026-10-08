package svcconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BlackVS/aicrew/internal/privatefile"
	"github.com/BlackVS/aicrew/internal/privatefile/privatefiletest"
)

const testHubID = "01a119aa-85ea-7000-80e5-1ee514e3db40"

func credential(n byte) string {
	return "aimem_peer_" + strings.Repeat(string("0123456789abcdef"[n]), 64)
}

// credDir writes what aimem identity peer provision writes: the hub ID and
// four private credential files, each different.
func credDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writePrivate(t, filepath.Join(dir, HubIDFile), testHubID+"\n")
	for i, name := range []string{RedemptionTokenFile, ReadTokenFile, TeamRegisterTokenFile, TeamReadTokenFile} {
		writePrivate(t, filepath.Join(dir, name), credential(byte(i+1))+"\n")
	}
	return dir
}

func writePrivate(t *testing.T, path, content string) {
	t.Helper()
	os.Remove(path)
	f, err := privatefile.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func hubRequest(name, dir string) HubRequest {
	return HubRequest{Name: name, BaseURL: "https://hub.example:8443", TLSTrustMode: "ca_dns",
		TLSTrustValue: "hub.example", CredDir: dir, Now: migrateNow}
}

// recordProbe records the hubs probed and answers err.
type recordProbe struct {
	hubs    []AimemHub
	service string
	err     error
}

func (p *recordProbe) probe(h AimemHub, serviceID string) error {
	p.hubs, p.service = append(p.hubs, h), serviceID
	return p.err
}

// A configuration with no hub gets one aimem_hubs entry: the hub ID read
// from the directory, every path absolute, the previous file kept, and the
// probe run with the entry before the write.
func TestAddHub(t *testing.T) {
	path := writeConfig(t, validConfig)
	dir := credDir(t)
	p := &recordProbe{}
	r, err := AddHub(path, hubRequest("main", dir), p.probe)
	if err != nil {
		t.Fatal(err)
	}
	want := AimemHub{Name: "main", HubID: testHubID, AimemConfig: AimemConfig{BaseURL: "https://hub.example:8443",
		TLSTrustMode: "ca_dns", TLSTrustValue: "hub.example",
		RedemptionTokenFile: filepath.Join(dir, RedemptionTokenFile), ReadTokenFile: filepath.Join(dir, ReadTokenFile)},
		TeamRegisterTokenFile: filepath.Join(dir, TeamRegisterTokenFile), TeamReadTokenFile: filepath.Join(dir, TeamReadTokenFile)}
	if r.Hub != want || r.Index != 0 || r.Updated || r.ReadScope != "" || len(r.Pending) != 0 || r.Backup != path+".20261008T043000Z.bak" {
		t.Fatalf("report = %+v", r)
	}
	if len(p.hubs) != 1 || p.hubs[0] != want || p.service != "aicrew-example" {
		t.Fatalf("probed %+v as %q", p.hubs, p.service)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AimemHubs) != 1 || c.AimemHubs[0] != want || !filepath.IsAbs(c.AimemHubs[0].TeamReadTokenFile) {
		t.Fatalf("config = %+v", c.AimemHubs)
	}
	if got := mustRead(t, r.Backup); string(got) != validConfig {
		t.Fatalf("backup = %s", got)
	}
}

// A relative --cred-dir is written as an absolute path.
func TestAddHubRelativeDir(t *testing.T) {
	path := writeConfig(t, validConfig)
	dir := credDir(t)
	t.Chdir(filepath.Dir(dir))
	r, err := AddHub(path, hubRequest("main", filepath.Base(dir)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Hub.RedemptionTokenFile != filepath.Join(dir, RedemptionTokenFile) {
		t.Fatalf("redemption file = %s", r.Hub.RedemptionTokenFile)
	}
}

// The same name again replaces its entry in place; another name adds a
// second entry, without the read credential, since one hub serves the read
// scope.
func TestAddHubUpdateAndSecond(t *testing.T) {
	path := writeConfig(t, validConfig)
	if _, err := AddHub(path, hubRequest("main", credDir(t)), nil); err != nil {
		t.Fatal(err)
	}
	other := credDir(t)
	writePrivate(t, filepath.Join(other, HubIDFile), "01a119aa-85ea-7000-80e5-000000000002\n")
	r, err := AddHub(path, hubRequest("second", other), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Updated || r.Index != 1 || r.ReadScope != "main" || r.Hub.ReadTokenFile != "" {
		t.Fatalf("second = %+v", r)
	}
	moved := credDir(t)
	req := hubRequest("main", moved)
	req.Now = migrateNow.Add(1e9)
	req.BaseURL, req.TLSTrustValue = "https://hub2.example", "hub2.example"
	r, err = AddHub(path, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Updated || r.Index != 0 || r.ReadScope != "" || r.Hub.ReadTokenFile != filepath.Join(moved, ReadTokenFile) {
		t.Fatalf("update = %+v", r)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AimemHubs) != 2 || c.AimemHubs[0].Name != "main" || c.AimemHubs[0].BaseURL != "https://hub2.example" ||
		c.AimemHubs[0].RedemptionTokenFile != filepath.Join(moved, RedemptionTokenFile) ||
		c.AimemHubs[1].Name != "second" || c.AimemHubs[1].HubID != "01a119aa-85ea-7000-80e5-000000000002" {
		t.Fatalf("hubs = %+v", c.AimemHubs)
	}
}

// A hub migrated without its hub_id gets it from hub add; another hub's
// missing hub_id is reported.
func TestAddHubCompletesAMigratedHub(t *testing.T) {
	path := writeConfig(t, legacyConfig)
	if _, err := MigrateConfig(path, MigrateOptions{Now: migrateNow}); err != nil {
		t.Fatal(err)
	}
	other := credDir(t)
	writePrivate(t, filepath.Join(other, HubIDFile), "01a119aa-85ea-7000-80e5-000000000002\n")
	req := hubRequest("second", other)
	req.Now = migrateNow.Add(1e9)
	r, err := AddHub(path, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.Pending, ",") != LegacyHubName || r.ReadScope != LegacyHubName {
		t.Fatalf("report = %+v", r)
	}
	req = hubRequest(LegacyHubName, credDir(t))
	req.Now = migrateNow.Add(2e9)
	if r, err = AddHub(path, req, nil); err != nil || !r.Updated || len(r.Pending) != 0 {
		t.Fatalf("report = %+v, %v", r, err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("the completed file is refused: %v", err)
	}
}

// Every refusal names its cause, writes nothing and probes nothing.
func TestAddHubRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		edit   func(t *testing.T, dir string)
		req    func(r *HubRequest)
		want   string
	}{
		{"no hub id", validConfig, func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, HubIDFile)) }, nil, HubIDFile},
		{"empty hub id", validConfig, func(t *testing.T, dir string) { writePrivate(t, filepath.Join(dir, HubIDFile), "\n") }, nil, HubIDFile + " is empty"},
		{"two-line hub id", validConfig, func(t *testing.T, dir string) {
			writePrivate(t, filepath.Join(dir, HubIDFile), testHubID+"\n"+testHubID+"\n")
		}, nil, HubIDFile + " must hold one value"},
		{"hub id not a UUID", validConfig, func(t *testing.T, dir string) { writePrivate(t, filepath.Join(dir, HubIDFile), "hub-1\n") }, nil, "does not hold a hub ID"},
		{"upper-case hub id", validConfig, func(t *testing.T, dir string) {
			writePrivate(t, filepath.Join(dir, HubIDFile), strings.ToUpper(testHubID)+"\n")
		}, nil, "does not hold a hub ID"},
		{"no read credential", validConfig, func(t *testing.T, dir string) { os.Remove(filepath.Join(dir, ReadTokenFile)) }, nil, ReadTokenFile},
		{"readable credential", validConfig, func(t *testing.T, dir string) {
			if err := privatefiletest.Expose(filepath.Join(dir, TeamReadTokenFile)); err != nil {
				t.Fatal(err)
			}
		}, nil, TeamReadTokenFile},
		{"two-line credential", validConfig, func(t *testing.T, dir string) {
			writePrivate(t, filepath.Join(dir, TeamRegisterTokenFile), credential(3)+"\n"+credential(3)+"\n")
		}, nil, TeamRegisterTokenFile + " must hold one value"},
		{"not a credential", validConfig, func(t *testing.T, dir string) {
			writePrivate(t, filepath.Join(dir, RedemptionTokenFile), "secret\n")
		}, nil, RedemptionTokenFile + " does not hold a peer credential"},
		{"same credential twice", validConfig, func(t *testing.T, dir string) {
			writePrivate(t, filepath.Join(dir, TeamReadTokenFile), credential(1)+"\n")
		}, nil, "hold the same credential"},
		{"legacy block", legacyConfig, nil, nil, "aicrewd config migrate"},
		{"bad name", validConfig, nil, func(r *HubRequest) { r.Name = "Main" }, "name"},
		{"plain http", validConfig, nil, func(r *HubRequest) { r.BaseURL = "http://hub.example" }, "base_url"},
		{"trust for another host", validConfig, nil, func(r *HubRequest) { r.TLSTrustValue = "other.example" }, "aimem_hubs[0]"},
		{"no cred dir", validConfig, nil, func(r *HubRequest) { r.CredDir = "" }, "--cred-dir"},
		{"hub id of another hub", "", nil, nil, "another hub's too"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := tc.config
			if config == "" {
				config = strings.Replace(validConfig, "}", `,"aimem_hubs":[{"name":"other","hub_id":"`+testHubID+
					`","base_url":"https://other.example","tls_trust_mode":"ca_dns","tls_trust_value":"other.example",`+
					`"redemption_token_file":"/etc/aicrew/r.token"}]}`, 1)
			}
			path := writeConfig(t, config)
			dir := credDir(t)
			if tc.edit != nil {
				tc.edit(t, dir)
			}
			req := hubRequest("main", dir)
			if tc.req != nil {
				tc.req(&req)
			}
			p := &recordProbe{}
			if _, err := AddHub(path, req, p.probe); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error naming %q", err, tc.want)
			}
			if string(mustRead(t, path)) != config || len(dirNames(t, path)) != 1 || len(p.hubs) != 0 {
				t.Fatalf("written or probed: files %v, probed %d", dirNames(t, path), len(p.hubs))
			}
		})
	}
}

// A refused probe writes nothing, and its error is returned as it is.
func TestAddHubProbeRefused(t *testing.T) {
	path := writeConfig(t, validConfig)
	p := &recordProbe{err: errors.New("the hub refused the team read with peer_forbidden: check service_id")}
	if _, err := AddHub(path, hubRequest("main", credDir(t)), p.probe); err == nil || !strings.Contains(err.Error(), "peer_forbidden") {
		t.Fatalf("got %v", err)
	}
	if string(mustRead(t, path)) != validConfig || len(dirNames(t, path)) != 1 {
		t.Fatal("written after a refused probe")
	}
}
