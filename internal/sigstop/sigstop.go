package sigstop

import (
	"os"
	"syscall"
	"time"
)

func Raise() {
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		panic(err)
	}
	if err := p.Signal(syscall.SIGSTOP); err != nil {
		panic(err)
	}
	// Wait for a bit to ensure the signal is delivered.
	time.Sleep(1 * time.Second)
}
