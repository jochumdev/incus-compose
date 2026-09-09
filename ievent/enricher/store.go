package enricher

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
)

// storeFileMode is what the fleet is written as. What a project sets is in it,
// so it is the daemon's business and nobody else's.
const storeFileMode = 0o600

// writeFunc puts one encoded fleet where the next start of this process will
// find it. A function rather than a path, so a test writes into memory instead
// of onto a disk.
type writeFunc func(b []byte) error

// fileWriter writes to path through a temporary beside it, so a reader never
// finds half a fleet. Not synced: the next clone says everything this one said,
// and a machine that went down has a run to do anyway.
func fileWriter(path string) writeFunc {
	return func(b []byte) error {
		tmp := path + ".tmp"

		err := os.WriteFile(tmp, b, storeFileMode)
		if err != nil {
			return fmt.Errorf("writing %s: %w", tmp, err)
		}

		err = os.Rename(tmp, path)
		if err != nil {
			return fmt.Errorf("renaming %s over %s: %w", tmp, path, err)
		}

		return nil
	}
}

// storeArgs is everything one store needs. A bundle rather than four
// parameters, since the goroutine and the fold both hold the same channels.
type storeArgs struct {
	// write is where an encoded fleet goes. A field rather than the path, so a
	// test answers without a disk.
	write writeFunc

	// in carries a clone the fold has finished with. One slot, newest wins: a
	// clone still waiting has been overtaken by the one behind it, and the fold
	// may not wait on a disk. Read-only here - displacing the stale one is the
	// fold's end of it, in storeSend.
	in <-chan *state

	// done is closed once the writer has stopped, so going down can wait for
	// what it handed over to land.
	done chan struct{}
}

// storeSend hands one clone over, and never blocks.
//
// A clone the slot already held is dropped rather than queued: it says less than
// the one replacing it, and a disk that cannot keep up must cost writes rather
// than cost the chain.
func storeSend(in chan *state, s *state) {
	select {
	case in <- s:
		return
	default:
	}

	// Take the stale one out and put this one in its place. Both halves are
	// offers rather than waits, so a writer that got there first simply wins.
	select {
	case <-in:
	default:
	}

	select {
	case in <- s:
	default:
	}
}

// runStore writes what it is handed for the life of ctx, on a goroutine of its
// own.
//
// The clone left in the slot when ctx ends is written before it stops, which is
// what makes the one offered on the way down the one the next start finds.
func runStore(ctx context.Context, logger *slog.Logger, a storeArgs) {
	go func() {
		defer close(a.done)

		for {
			select {
			case s := <-a.in:
				storeWrite(logger, a.write, s)

			case <-ctx.Done():
				select {
				case s := <-a.in:
					storeWrite(logger, a.write, s)
				default:
				}

				return
			}
		}
	}()
}

// storeWrite encodes one clone and puts it away.
//
// A failure is logged and left. The next clone says everything this one said,
// and a fleet that could not be written is a slower start next time rather than
// anything the chain should hear about.
func storeWrite(logger *slog.Logger, write writeFunc, s *state) {
	b, err := json.Marshal(s)
	if err != nil {
		logger.Warn("the fleet could not be encoded, keeping what was last written",
			"plugin", name, "err", err)

		return
	}

	err = write(b)
	if err != nil {
		logger.Warn("the fleet could not be written, keeping what was last written",
			"plugin", name, "err", err)
	}
}
