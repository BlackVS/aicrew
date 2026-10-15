package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/BlackVS/aicrew/internal/filelock"
	"github.com/BlackVS/aicrew/internal/managedfiles"
)

// RegistryEnv names another registry file, for tests.
const RegistryEnv = "AICREW_SERVICE_REGISTRY"

// Entry is one registered home.
type Entry struct {
	Home  string `json:"home"`
	Label string `json:"label"`
	Name  string `json:"name"`
}

// Registry is the per-user list of the homes that have a service:
// <user config dir>/aicrew/services.json.
type Registry struct{ Path string }

// DefaultRegistry is the member's registry, or RegistryEnv's file.
func DefaultRegistry(getenv func(string) string) (Registry, error) {
	if p := getenv(RegistryEnv); p != "" {
		return Registry{Path: p}, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return Registry{}, err
	}
	return Registry{Path: filepath.Join(dir, "aicrew", "services.json")}, nil
}

// Dir is the registry's directory, where the Windows task definitions are
// kept as well.
func (r Registry) Dir() string { return filepath.Dir(r.Path) }

type registryDoc struct {
	Services []Entry `json:"services"`
}

// Entries lists the registered homes, by home.
func (r Registry) Entries() ([]Entry, error) {
	b, err := os.ReadFile(r.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d registryDoc
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, errors.New("the service registry " + r.Path + " is not valid JSON: " + err.Error())
	}
	return d.Services, nil
}

// Lookup is the home's entry, if registered.
func (r Registry) Lookup(home string) (Entry, bool, error) {
	all, err := r.Entries()
	if err != nil {
		return Entry{}, false, err
	}
	for _, e := range all {
		if samePath(e.Home, home) {
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

// Put records the home's entry, replacing an earlier one.
func (r Registry) Put(e Entry) error {
	return r.edit(func(all []Entry) []Entry {
		out := []Entry{e}
		for _, x := range all {
			if !samePath(x.Home, e.Home) {
				out = append(out, x)
			}
		}
		return out
	})
}

// Remove forgets the home.
func (r Registry) Remove(home string) error {
	return r.edit(func(all []Entry) []Entry {
		var out []Entry
		for _, x := range all {
			if !samePath(x.Home, home) {
				out = append(out, x)
			}
		}
		return out
	})
}

func (r Registry) edit(f func([]Entry) []Entry) error {
	if err := os.MkdirAll(r.Dir(), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	unlock, err := filelock.Lock(ctx, r.Path+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	all, err := r.Entries()
	if err != nil {
		return err
	}
	all = f(all)
	sort.Slice(all, func(i, j int) bool { return all[i].Home < all[j].Home })
	b, err := json.MarshalIndent(registryDoc{Services: all}, "", "  ")
	if err != nil {
		return err
	}
	return managedfiles.WriteAtomic(r.Path, append(b, '\n'))
}

// samePath compares two homes as the service names do.
func samePath(a, b string) bool { return Name(a, "") == Name(b, "") }
