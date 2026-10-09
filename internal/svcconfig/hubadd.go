package svcconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/BlackVS/aicrew/internal/privatefile"
)

// The files `aimem identity peer provision --output-dir DIR` writes: the
// hub's ID and the five peer credentials it issues this service, under
// fixed names. aimem 0.10.0 added the fifth, board.read.
const (
	HubIDFile             = "aimem-hub-id"
	RedemptionTokenFile   = "aimem-redeem.token"
	ReadTokenFile         = "aimem-read.token"
	TeamRegisterTokenFile = "aimem-team-register.token"
	TeamReadTokenFile     = "aimem-team-read.token"
	BoardReadTokenFile    = "aimem-board-read.token"
)

// HubRequest is one hub as the operator names it: its alias, how to reach
// and trust it, and the directory aimem provisioned its files into.
type HubRequest struct {
	Name          string
	BaseURL       string
	TLSTrustMode  string
	TLSTrustValue string
	CredDir       string
	// Now stamps the copy of the previous file.
	Now time.Time
}

// HubReport is what AddHub did.
type HubReport struct {
	// Hub is the entry written, and Index its place in aimem_hubs.
	Hub   AimemHub
	Index int
	// Updated is true when an entry of that name was replaced in place.
	Updated bool
	// Backup is the copy of the previous file.
	Backup string
	// ReadScope names the other hub that serves the reservation read scope
	// when there is one: this entry then has no read_token_file, since only
	// one hub may.
	ReadScope string
	// Pending names the other hubs that still have no hub_id.
	Pending []string
}

// Probe checks the hub with the entry about to be written, live, before
// anything is written: aicrew hub add reads the peer's teams with its
// team.read credential.
type Probe func(h AimemHub, serviceID string) error

// hubIDShape is a hub's ID as aimem writes it: a UUID in lowercase
// canonical form.
var hubIDShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// peerCredentialShape is an aimem peer credential.
var peerCredentialShape = regexp.MustCompile(`^aimem_peer_[0-9a-f]{64}$`)

// maxCredFile bounds what is read of a provisioned file.
const maxCredFile = 4 << 10

// AddHub adds the hub req names to the aimem_hubs of the configuration at
// path, or replaces the entry of that name in place. The hub ID and the
// credentials come from req.CredDir: each credential file must be private
// and hold one peer credential, all five different, and the hub ID file
// one UUID. Every path written is absolute. The file must already be in
// the aimem_hubs form (or have no hub): a legacy aimem block is moved
// first with aicrewd config migrate. The rewritten file is checked as
// aicrewd reads it, then probe runs, and only then is the file replaced,
// its previous version kept beside it. A refusal writes nothing.
func AddHub(path string, req HubRequest, probe Probe) (HubReport, error) {
	if !hubNameShape.MatchString(req.Name) {
		return HubReport{}, errors.New("the hub's name must be 1 to 32 lowercase letters, digits or '-'")
	}
	path, raw, info, err := openForEdit(path)
	if err != nil {
		return HubReport{}, err
	}
	c, err := parsePending(raw)
	if err != nil {
		return HubReport{}, err
	}
	if c.Aimem != nil {
		return HubReport{}, errors.New("config still has the aimem block of 0.2.0: run aicrewd config migrate first")
	}
	hub, err := provisioned(req)
	if err != nil {
		return HubReport{}, err
	}
	r := HubReport{Index: len(c.AimemHubs)}
	for i, h := range c.AimemHubs {
		switch {
		case h.Name == req.Name:
			r.Index, r.Updated = i, true
		case h.ReadTokenFile != "":
			r.ReadScope = h.Name
		}
		if h.Name != req.Name && h.HubID == "" {
			r.Pending = append(r.Pending, h.Name)
		}
	}
	if r.ReadScope != "" {
		hub.ReadTokenFile = ""
	}
	r.Hub = hub
	want := c
	want.AimemHubs = append([]AimemHub(nil), c.AimemHubs...)
	if r.Updated {
		want.AimemHubs[r.Index] = hub
	} else {
		want.AimemHubs = append(want.AimemHubs, hub)
	}
	out, err := withHub(raw, hub, r.Index, r.Updated)
	if err != nil {
		return HubReport{}, err
	}
	nc, err := checkEdited(out)
	if err != nil {
		return HubReport{}, err
	}
	if !reflect.DeepEqual(nc, want) {
		return HubReport{}, errors.New("the rewritten config would not read as the original with this hub added; nothing was written")
	}
	if probe != nil {
		if err := probe(hub, c.ServiceID); err != nil {
			return HubReport{}, err
		}
	}
	if r.Backup, err = commitEdit(path, raw, out, info, req.Now); err != nil {
		return HubReport{}, err
	}
	return r, nil
}

// provisioned is the entry req describes, its hub ID and credential files
// read from req.CredDir and checked. Errors name the file, never its
// content.
func provisioned(req HubRequest) (AimemHub, error) {
	if req.CredDir == "" {
		return AimemHub{}, errors.New("--cred-dir is required: the directory aimem identity peer provision wrote")
	}
	dir, err := filepath.Abs(req.CredDir)
	if err != nil {
		return AimemHub{}, err
	}
	hubID, err := readOneLine(filepath.Join(dir, HubIDFile))
	if err != nil {
		return AimemHub{}, err
	}
	if !hubIDShape.MatchString(hubID) {
		return AimemHub{}, fmt.Errorf("%s does not hold a hub ID (a UUID in lowercase, as aimem identity peer provision writes it)", filepath.Join(dir, HubIDFile))
	}
	h := AimemHub{Name: req.Name, HubID: hubID, AimemConfig: AimemConfig{BaseURL: req.BaseURL,
		TLSTrustMode: req.TLSTrustMode, TLSTrustValue: req.TLSTrustValue}}
	seen := map[string]string{}
	for _, f := range []struct {
		name string
		dst  *string
	}{
		{RedemptionTokenFile, &h.RedemptionTokenFile},
		{ReadTokenFile, &h.ReadTokenFile},
		{TeamRegisterTokenFile, &h.TeamRegisterTokenFile},
		{TeamReadTokenFile, &h.TeamReadTokenFile},
		{BoardReadTokenFile, &h.BoardReadTokenFile},
	} {
		p := filepath.Join(dir, f.name)
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) && f.name == BoardReadTokenFile {
			return AimemHub{}, fmt.Errorf("%s is missing: the directory was provisioned before aimem 0.10.0; run aimem identity "+
				"peer provision again with the same arguments, which issues only the missing board.read credential", p)
		}
		if err := privatefile.Check(p); err != nil {
			return AimemHub{}, err
		}
		cred, err := readOneLine(p)
		if err != nil {
			return AimemHub{}, err
		}
		if !peerCredentialShape.MatchString(cred) {
			return AimemHub{}, fmt.Errorf("%s does not hold a peer credential (aimem_peer_ and 64 lowercase hex)", p)
		}
		if other, ok := seen[cred]; ok {
			return AimemHub{}, fmt.Errorf("%s and %s hold the same credential; aimem issues one per operation", other, p)
		}
		seen[cred] = p
		*f.dst = p
	}
	return h, nil
}

// readOneLine returns the one line path holds, without its line ending.
// Errors name the file, never its content.
func readOneLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxCredFile+1))
	if err != nil {
		return "", err
	}
	if len(raw) > maxCredFile {
		return "", fmt.Errorf("%s is larger than one line", path)
	}
	s := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return "", fmt.Errorf("%s must hold one value alone on one line", path)
		}
	}
	return s, nil
}

// withHub is raw with hub written into aimem_hubs: at index in place of the
// entry there when replace is true, else appended. Every other member, and
// every other entry, keeps its raw value and place; a configuration with
// no aimem_hubs gets one at its end.
func withHub(raw []byte, hub AimemHub, index int, replace bool) ([]byte, error) {
	top, err := decodeJSONObject(raw)
	if err != nil {
		return nil, err
	}
	entry, err := json.Marshal(hub)
	if err != nil {
		return nil, err
	}
	var hubs []json.RawMessage
	if v := top.get("aimem_hubs"); v != nil && string(v) != "null" {
		if err := json.Unmarshal(v, &hubs); err != nil {
			return nil, fmt.Errorf("config: aimem_hubs: %w", err)
		}
	}
	if replace {
		hubs[index] = entry
	} else {
		hubs = append(hubs, entry)
	}
	list, err := json.Marshal(hubs)
	if err != nil {
		return nil, err
	}
	if top.get("aimem_hubs") != nil {
		top.replace("aimem_hubs", "aimem_hubs", list)
	} else {
		top = append(top, jsonMember{"aimem_hubs", list})
	}
	return indent(top.encode())
}
