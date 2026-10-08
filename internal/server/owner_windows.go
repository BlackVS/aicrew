package server

import "os"

// sameOwner keeps nothing on Windows: the replacement inherits the
// directory's access control list.
func sameOwner(*os.File, os.FileInfo) error { return nil }
