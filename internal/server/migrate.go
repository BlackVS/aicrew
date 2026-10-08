package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// MigrateOptions are the values `aicrewd config migrate` adds to a legacy
// aimem block while it moves it into aimem_hubs. Each is optional.
type MigrateOptions struct {
	// Name is the hub's alias; empty means LegacyHubName, the name the
	// block is read under today.
	Name                  string
	HubID                 string
	TeamRegisterTokenFile string
	TeamReadTokenFile     string
	// ServiceID replaces service_id when set.
	ServiceID string
	// Now stamps the copy of the previous file.
	Now time.Time
}

func (o MigrateOptions) any() bool {
	return o.Name != "" || o.HubID != "" || o.TeamRegisterTokenFile != "" || o.TeamReadTokenFile != "" || o.ServiceID != ""
}

// MigrateReport is what a migration did and what it leaves to the operator.
type MigrateReport struct {
	// Migrated is true when the file was rewritten; Backup is then the copy
	// of the previous file.
	Migrated bool
	Backup   string
	// Missing names each field the operator still has to supply.
	Missing []MissingField
	// ServiceID is the configuration's service_id, which must be the peer ID
	// each hub lists for this service; migrate cannot check it.
	ServiceID string
	// Hubs is the number of configured hubs.
	Hubs int
}

// MissingField is a field still to supply and where its value comes from.
// Required is true when aicrewd refuses the file without it.
type MissingField struct {
	Field    string
	From     string
	Required bool
}

// Incomplete is true while a hub has no hub_id: aicrewd refuses the file
// until it is set.
func (r MigrateReport) Incomplete() bool {
	for _, m := range r.Missing {
		if m.Required {
			return true
		}
	}
	return false
}

// MigrateConfig rewrites the configuration at path from the single aimem
// block of 0.2.0 into one aimem_hubs entry. The block's fields move as they
// are, every other field keeps its value and place, and the previous file
// is kept beside it as path.<UTC time>.bak. A file already in the
// aimem_hubs form, or with no hub at all, is left untouched. Either way the
// report names the fields still missing. A file aicrewd would refuse for
// any other reason than a missing hub_id is refused, and nothing is
// written. A symbolic link is followed: the file it names is migrated.
func MigrateConfig(path string, opt MigrateOptions) (MigrateReport, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if path, err = filepath.EvalSymlinks(path); err != nil {
			return MigrateReport{}, fmt.Errorf("open config: %w", err)
		}
	}
	raw, info, err := readConfigFile(path)
	if err != nil {
		return MigrateReport{}, err
	}
	if !info.Mode().IsRegular() {
		return MigrateReport{}, errors.New("config is not a regular file")
	}
	c, err := parsePending(raw)
	if err != nil {
		return MigrateReport{}, err
	}
	if c.Aimem == nil {
		if opt.any() {
			return MigrateReport{}, errors.New("config has no aimem block: migrate changes nothing, and its flags apply only to that block")
		}
		return report(c), nil
	}
	if opt.Name == "" {
		opt.Name = LegacyHubName
	}
	for _, f := range []*string{&opt.TeamRegisterTokenFile, &opt.TeamReadTokenFile} {
		if *f != "" {
			if *f, err = filepath.Abs(*f); err != nil {
				return MigrateReport{}, err
			}
		}
	}
	out, err := migrated(raw, opt)
	if err != nil {
		return MigrateReport{}, err
	}
	if len(out) > maxConfigBytes {
		return MigrateReport{}, fmt.Errorf("the migrated config would be refused: it is larger than %d bytes", maxConfigBytes)
	}
	if _, err := decodeJSONObject(out); err != nil {
		return MigrateReport{}, fmt.Errorf("the migrated config would be refused: %w", err)
	}
	nc, err := parsePending(out)
	if err != nil {
		return MigrateReport{}, fmt.Errorf("the migrated config would be refused: %w", err)
	}
	// aicrewd must read the new file as the old one with the block moved:
	// a member the text rewrite missed would otherwise change what it reads.
	want := c
	want.Aimem = nil
	want.AimemHubs = []AimemHub{{Name: opt.Name, HubID: opt.HubID, AimemConfig: *c.Aimem,
		TeamRegisterTokenFile: opt.TeamRegisterTokenFile, TeamReadTokenFile: opt.TeamReadTokenFile}}
	if opt.ServiceID != "" {
		want.ServiceID = opt.ServiceID
	}
	if !reflect.DeepEqual(nc, want) {
		return MigrateReport{}, errors.New("the migrated config would not read as the original with its aimem block moved; move the block by hand")
	}
	backup := path + "." + opt.Now.UTC().Format("20060102T150405Z") + ".bak"
	if err := writeBackup(backup, raw, info.Mode().Perm()); err != nil {
		return MigrateReport{}, err
	}
	if err := replaceFile(path, out, info); err != nil {
		return MigrateReport{}, err
	}
	r := report(nc)
	r.Migrated, r.Backup = true, backup
	return r, nil
}

func readConfigFile(path string) ([]byte, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("read config: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read config: %w", err)
	}
	if len(raw) > maxConfigBytes {
		return nil, nil, fmt.Errorf("config is larger than %d bytes", maxConfigBytes)
	}
	return raw, info, nil
}

// parsePending is ParseConfig, except that an aimem_hubs entry may still
// lack its hub_id: that is what a migration without -hub-id writes.
func parsePending(raw []byte) (Config, error) {
	c, err := decodeConfig(raw)
	if err != nil {
		return Config{}, err
	}
	check := c
	check.AimemHubs = append([]AimemHub(nil), c.AimemHubs...)
	for i := range check.AimemHubs {
		if check.AimemHubs[i].HubID == "" {
			check.AimemHubs[i].HubID = fmt.Sprintf("pending.hub_id.%d", i)
		}
	}
	return c, check.validate()
}

func report(c Config) MigrateReport {
	r := MigrateReport{ServiceID: c.ServiceID, Hubs: len(c.AimemHubs)}
	for i, h := range c.AimemHubs {
		at := fmt.Sprintf("aimem_hubs[%d]", i)
		if h.HubID == "" {
			r.Missing = append(r.Missing, MissingField{at + ".hub_id", "the hub's ID, as `aimem identity peer list` shows it on the hub", true})
		}
		if h.TeamRegisterTokenFile == "" {
			r.Missing = append(r.Missing, MissingField{at + ".team_register_token_file",
				"a file holding the credential from `aimem identity cred issue --peer SERVICE_ID --operation team.register`", false})
		}
		if h.TeamReadTokenFile == "" {
			r.Missing = append(r.Missing, MissingField{at + ".team_read_token_file",
				"a file holding the credential from `aimem identity cred issue --peer SERVICE_ID --operation team.read`", false})
		}
	}
	return r
}

// migrated is raw with its aimem member replaced, in its place, by
// aimem_hubs holding one entry: name and hub_id, the block's own fields in
// their order, then the team credential files.
func migrated(raw []byte, opt MigrateOptions) ([]byte, error) {
	top, err := decodeJSONObject(raw)
	if err != nil {
		return nil, err
	}
	if top.get("aimem_hubs") != nil {
		// Even an empty or null aimem_hubs would be a second member of that
		// name, and the later one wins when aicrewd reads the file.
		return nil, errors.New("config: aimem and aimem_hubs are given together; remove the empty aimem_hubs or move the aimem block into it")
	}
	block, err := decodeJSONObject(top.get("aimem"))
	if err != nil {
		return nil, fmt.Errorf("config: aimem: %w", err)
	}
	var entry jsonObject
	entry.add("name", opt.Name)
	if opt.HubID != "" {
		entry.add("hub_id", opt.HubID)
	}
	entry = append(entry, block...)
	for _, f := range []struct{ key, path string }{
		{"team_register_token_file", opt.TeamRegisterTokenFile},
		{"team_read_token_file", opt.TeamReadTokenFile},
	} {
		if f.path != "" {
			entry.add(f.key, f.path)
		}
	}
	hubs, err := json.Marshal([]json.RawMessage{entry.encode()})
	if err != nil {
		return nil, err
	}
	top.replace("aimem", "aimem_hubs", hubs)
	if opt.ServiceID != "" {
		v, _ := json.Marshal(opt.ServiceID)
		top.replace("service_id", "service_id", v)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, top.encode(), "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// jsonObject is a JSON object's members in their order.
type jsonObject []jsonMember

type jsonMember struct {
	key   string
	value json.RawMessage
}

func decodeJSONObject(raw []byte) (jsonObject, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("a JSON object expected")
	}
	var o jsonObject
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if o.get(t.(string)) != nil {
			return nil, fmt.Errorf("%q is given twice (names are matched regardless of case)", t)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o = append(o, jsonMember{t.(string), v})
	}
	return o, nil
}

// get and replace match a key regardless of case, as encoding/json matches
// a member to a Config field.
func (o jsonObject) get(key string) json.RawMessage {
	for _, m := range o {
		if strings.EqualFold(m.key, key) {
			return m.value
		}
	}
	return nil
}

func (o *jsonObject) add(key, value string) {
	v, _ := json.Marshal(value)
	*o = append(*o, jsonMember{key, v})
}

func (o jsonObject) replace(key, newKey string, value json.RawMessage) {
	for i := range o {
		if strings.EqualFold(o[i].key, key) {
			o[i] = jsonMember{newKey, value}
		}
	}
}

func (o jsonObject) encode() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(m.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// writeBackup creates path, which must not exist, holding raw.
func writeBackup(path string, raw []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("keep the previous config: %w", err)
	}
	_, werr := f.Write(raw)
	if err := errors.Join(werr, f.Close()); err != nil {
		os.Remove(path)
		return fmt.Errorf("keep the previous config: %w", err)
	}
	return nil
}

// replaceFile replaces path with data atomically, keeping the mode and, where
// the platform has them, the owner and group of the file it replaces.
func replaceFile(path string, data []byte, prev os.FileInfo) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	_, werr := tmp.Write(data)
	err = errors.Join(werr, tmp.Chmod(prev.Mode().Perm()), sameOwner(tmp, prev), tmp.Sync(), tmp.Close())
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
