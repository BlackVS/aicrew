package server

import (
	. "github.com/BlackVS/aicrew/internal/store"
)

// A fixture of TestExposureGuardRejectsBypasses: a dot import constructs the
// operator's caller outside the wrapper.
func bypassDotImport() (Caller, error) { return OperatorCaller("operator") }
