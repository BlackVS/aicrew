//go:build windows

package agent

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// limitMemory puts this process in a job object of its own whose process
// memory limit is limit: the committed memory Windows lets it hold. A
// commit past it fails, and the Go runtime ends the process ("VirtualAlloc
// ... failed", "fatal error: out of memory"). The job nests inside the
// launcher's kill-on-close job (launch_windows.go), in either order. Its
// handle stays open for the process's life.
func limitMemory(limit int64) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create a job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY},
		ProcessMemoryLimit:    uintptr(limit),
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("set the job object's memory limit: %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("enter the job object: %w", err)
	}
	return nil
}
