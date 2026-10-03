package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BlackVS/aicrew/internal/privatefile"
)

// Provisioning the home's aimem installation (D-STORE PR2): on a first join,
// given the hub's URL, the member's user-scoped token and, for a hub on a
// private CA, its CA file, join gives the home's installation the hub and the
// member's task credential itself, so the operator never runs aimem with
// hand-set variables. The token reaches aimem only on its standard input;
// the CA is copied into the home's creds/, so no path in the installation
// points outside the home. A home whose credential is already set and active
// is left as it is.

// tokenPrefix is aimem's prefix for every token it issues: a value with it
// is a token, never a file name.
const tokenPrefix = "aimem_"

// userTokenPrefix is the user-scoped token the task credential must be.
const userTokenPrefix = "aimem_user_"

// maxToken bounds a token file's content.
const maxToken = 1024

// checkProvisionOptions validates the provisioning flags.
func (o *JoinOptions) checkProvisionOptions(linked bool) error {
	if o.AimemURL == "" && o.AimemTokenFile == "" && o.AimemCAFile == "" {
		return nil
	}
	if linked {
		return errors.New("-aimem-url, -aimem-token-file and -aimem-ca-file provision a new home; this home is already linked")
	}
	if o.AimemURL == "" || o.AimemTokenFile == "" {
		return errors.New("-aimem-url and -aimem-token-file go together: the hub's origin and the member's token")
	}
	if tokenLike(o.AimemTokenFile) {
		return errors.New("-aimem-token-file names a file holding the token, or - for the hidden prompt: a token is never an argument")
	}
	u, err := url.Parse(o.AimemURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || strings.ContainsAny(o.AimemURL, "?#") ||
		(u.Path != "" && u.Path != "/") {
		return errors.New("-aimem-url must be the hub's https origin, without credentials, path, query or fragment")
	}
	o.AimemURL = strings.TrimSuffix(o.AimemURL, "/")
	return nil
}

// tokenLike reports whether a -aimem-token-file value is a token rather
// than a file: it has aimem's token prefix and names no existing file.
func tokenLike(v string) bool {
	if !strings.HasPrefix(v, tokenPrefix) {
		return false
	}
	_, err := os.Stat(v)
	return err != nil
}

// provision gives an unlinked home's installation the hub and the member's
// credential, unless it already has an active one.
func (j *joiner) provision(ctx context.Context) (JoinReport, bool) {
	o := j.o
	if o.AimemURL == "" {
		return JoinReport{}, false
	}
	if st, known, err := j.aimem.Credential(ctx); err == nil && known && st.Credential == "set" && st.State == "active" {
		fmt.Fprintf(j.deps.Out, "The home's aimem installation already holds an active credential for hub %q: not provisioning it again.\n", o.AimemHub)
		return JoinReport{}, false
	}
	token, err := j.readToken()
	if err != nil {
		return blocked(o, "aimem_token", err.Error()), true
	}
	for _, d := range []string{AimemDir(o.Home), filepath.Join(o.Home, "creds")} {
		if err := privatefile.MakeDir(d); err != nil {
			return blocked(o, "aimem_provision_failed", fmt.Sprintf("make %s owner-only: %v", d, err)), true
		}
	}
	ca := ""
	if o.AimemCAFile != "" {
		if ca, err = copyCA(o.AimemCAFile, filepath.Join(o.Home, "creds", "aimem."+o.AimemHub+".ca.pem")); err != nil {
			return blocked(o, "aimem_provision_failed", err.Error()), true
		}
	}
	if err := j.aimem.Provision(ctx, o.AimemURL, ca, token); err != nil {
		return blocked(o, "aimem_provision_failed", err.Error()+"; fix it and rerun join with the same flags"), true
	}
	fmt.Fprintf(j.deps.Out, "Provisioned the home's aimem installation with hub %q and the member's credential.\n", o.AimemHub)
	return JoinReport{}, false
}

// readToken reads the member's token from its file or the hidden prompt.
// Its errors never quote the content.
func (j *joiner) readToken() (string, error) {
	var raw []byte
	if j.o.AimemTokenFile == "-" {
		if j.deps.ReadToken == nil {
			return "", errors.New("the token can be read at a prompt only on a terminal")
		}
		s, err := j.deps.ReadToken()
		if err != nil {
			return "", fmt.Errorf("read the token: %w", err)
		}
		raw = []byte(s)
	} else {
		path := j.o.AimemTokenFile
		if err := privatefile.Check(path); err != nil {
			return "", fmt.Errorf("the token file must be readable by its owner only: %w", err)
		}
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		if raw, err = io.ReadAll(io.LimitReader(f, maxToken+1)); err != nil {
			return "", err
		}
		if len(raw) > maxToken {
			return "", fmt.Errorf("%s is larger than a token", path)
		}
	}
	s := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return "", errors.New("the token must be alone on one line")
		}
	}
	if !strings.HasPrefix(s, userTokenPrefix) {
		return "", errors.New("the token must be the member's user-scoped aimem token (aimem_user_...)")
	}
	return s, nil
}

// copyCA copies the hub's CA bundle into the home, owner-only, and returns
// the copy's absolute path. An identical copy is kept; a different one is
// replaced.
func copyCA(src, dst string) (string, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("read -aimem-ca-file: %w", err)
	}
	if !bytes.Contains(data, []byte("-----BEGIN CERTIFICATE-----")) {
		return "", errors.New("-aimem-ca-file holds no PEM certificate")
	}
	abs, err := filepath.Abs(dst)
	if err != nil {
		return "", err
	}
	if cur, err := os.ReadFile(abs); err == nil && bytes.Equal(cur, data) {
		return abs, nil
	}
	tmp := abs + ".new"
	os.Remove(tmp)
	f, err := privatefile.Create(tmp)
	if err != nil {
		return "", err
	}
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Sync(), f.Close()); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, abs); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return abs, nil
}

// Provision runs aimem's own provisioning against the home's installation:
// the hub, then the task credential, each with the token on standard input.
func (e joinExec) Provision(ctx context.Context, hubURL, caFile, token string) error {
	if e.Hub == "" {
		return errors.New("the aimem hub name is required")
	}
	add := []string{"hub", "add", e.Hub, hubURL, "--token-file", "-"}
	if caFile != "" {
		add = append(add, "--ca-file", caFile)
	}
	if _, err := e.run(ctx, token, add...); err != nil {
		return err
	}
	_, err := e.run(ctx, token, "hub", "task-token", e.Hub, "--token-file", "-")
	return err
}
