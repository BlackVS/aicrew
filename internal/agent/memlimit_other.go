//go:build !(linux || windows || darwin)

package agent

// limitMemory watches the Go runtime's memory: no OS limit is set on these
// platforms.
func limitMemory(limit int64) error {
	watchMemory(limit)
	return nil
}
