package agent

import (
	"fmt"
	"os"
	"runtime/metrics"
	"time"
)

// memoryWatchdogMessage is what the watchdog writes before it ends the
// process; the launcher reads it as a memory failure.
const memoryWatchdogMessage = "aicrew-agent automation: the memory limit was passed"

// watchMemory ends the process when the memory its Go runtime holds from
// the OS passes limit. It is the limit on the platforms whose kernel does
// not enforce RLIMIT_DATA for the runtime's mappings (memlimit_darwin.go,
// memlimit_other.go). It samples every millisecond: a script allocates
// through the interpreter one step at a time, and Starlark bounds what one
// step may allocate.
func watchMemory(limit int64) {
	samples := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	go func() {
		for {
			metrics.Read(samples)
			if held := int64(samples[0].Value.Uint64() - samples[1].Value.Uint64()); held > limit {
				fmt.Fprintf(os.Stderr, "%s: %d bytes held, the limit is %d\n", memoryWatchdogMessage, held, limit)
				os.Exit(3)
			}
			time.Sleep(time.Millisecond)
		}
	}()
}
