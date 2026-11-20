package docsteps

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadScenarios reads the scenarios' declarations (scenarios.json): for each
// scenario, the ids of the document's steps its run executes. A run checks
// its own list at its end (docstep verify), and the package's test checks
// that the lists together cover every marked step not exempted.
func LoadScenarios(path string) (map[string][]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var decl map[string][]string
	if err := json.Unmarshal(raw, &decl); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return decl, nil
}
