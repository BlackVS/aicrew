// Command lingerversion stands in for aicrew-agent in TestBootPs1VersionRead:
// `version` prints a version line as aicrew-agent does, then waits
// LINGER_MS milliseconds before exiting with EXIT_CODE.
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

func main() {
	ms, _ := strconv.Atoi(os.Getenv("LINGER_MS"))
	code, _ := strconv.Atoi(os.Getenv("EXIT_CODE"))
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println("aicrew-agent v0.0.1 (lingerversion)")
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
	os.Exit(code)
}
