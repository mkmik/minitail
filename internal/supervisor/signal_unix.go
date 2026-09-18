//go:build unix

package supervisor

import (
	"os"
	"syscall"
)

// interruptSignal asks the child to shut down cleanly.
var interruptSignal os.Signal = syscall.SIGTERM
