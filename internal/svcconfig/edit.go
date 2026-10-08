package svcconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// openForEdit reads the configuration an operator command is about to
// rewrite. A symbolic link is followed, so the file it names is the one
// rewritten and the link stays; the path returned is that file's.
func openForEdit(path string) (string, []byte, os.FileInfo, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if path, err = filepath.EvalSymlinks(path); err != nil {
			return "", nil, nil, fmt.Errorf("open config: %w", err)
		}
	}
	raw, info, err := readConfigFile(path)
	if err != nil {
		return "", nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return "", nil, nil, errors.New("config is not a regular file")
	}
	return path, raw, info, nil
}

// checkEdited is the rewritten file as aicrewd would read it, refused if
// aicrewd would not: too large, a member named twice, or a check failing
// for any other reason than a hub_id still to supply.
func checkEdited(out []byte) (Config, error) {
	if len(out) > MaxConfigBytes {
		return Config{}, fmt.Errorf("the rewritten config would be refused: it is larger than %d bytes", MaxConfigBytes)
	}
	if _, err := decodeJSONObject(out); err != nil {
		return Config{}, fmt.Errorf("the rewritten config would be refused: %w", err)
	}
	c, err := parsePending(out)
	if err != nil {
		return Config{}, fmt.Errorf("the rewritten config would be refused: %w", err)
	}
	return c, nil
}

// commitEdit keeps raw beside path as path.<UTC time>.bak (or -2.bak and
// on, when a copy of that second exists), then replaces
// path with out atomically. It returns the copy's path.
func commitEdit(path string, raw, out []byte, info os.FileInfo, now time.Time) (string, error) {
	backup, err := writeBackup(path+"."+now.UTC().Format("20060102T150405Z"), raw, info.Mode().Perm())
	if err != nil {
		return "", err
	}
	if err := replaceFile(path, out, info); err != nil {
		return "", err
	}
	return backup, nil
}

// indent is a rewritten file's text: two-space indented, ending in a newline.
func indent(compact []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, compact, "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}
