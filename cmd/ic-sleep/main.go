// ic-sleep blocks so a one-off instance has a live process to exec into.
package main

import (
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// Waiting on a signal rather than an empty select: the runtime calls that a
	// deadlock and panics. It also makes `incus stop` return at once.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
}
