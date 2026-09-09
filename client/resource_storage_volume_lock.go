package client

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/pkg/sftp"

	"github.com/lxc/incus-compose/shared"
)

// SFTP returns a new SFTP connection to the volume. The caller closes it.
func (r *StorageVolume) SFTP(ctx context.Context) (*sftp.Client, error) {
	if !r.IsEnsured() {
		return nil, ErrNotEnsured.WithResource(r)
	}

	conn, err := r.client.Connection()
	if err != nil {
		return nil, err
	}

	return conn.GetStoragePoolVolumeFileSFTP(ctx, r.client.incusProject, r.Config.Pool, "custom", r.incusName)
}

// VolumeLock is an advisory lock on a file inside a StorageVolume; release it with Unlock.
type VolumeLock struct {
	client *Client
	sc     *sftp.Client
	path   string
	owner  string

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Lock acquires the named advisory lock on the volume, blocking until it is held or ctx is done.
// The name may contain slashes; missing parent directories are created.
// A stale of 0 means the lock is never taken over and the holder does not refresh it. sc must stay
// open until Unlock is called - the acquire uses it, and when stale > 0 so does the heartbeat.
func (r *StorageVolume) Lock(ctx context.Context, sc *sftp.Client, name string, stale time.Duration) (*VolumeLock, error) {
	if !r.IsEnsured() {
		return nil, ErrNotEnsured.WithResource(r)
	}

	lockPath := path.Join("/", name)

	dir := path.Dir(lockPath)
	if dir != "/" {
		err := sc.MkdirAll(dir)
		if err != nil {
			return nil, ErrOperation.WithText("creating lock directory " + dir).Wrap(err)
		}
	}

	hostname, _ := os.Hostname()
	owner := fmt.Sprintf("%s:%d:%s", hostname, os.Getpid(), shared.RandString(8))

	err := retry.New(
		retry.Context(ctx),
		retry.Attempts(0),
		retry.Delay(250*time.Millisecond),
		retry.DelayType(retry.FixedDelay),
		retry.LastErrorOnly(true),
	).Do(func() error {
		f, err := sc.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
		if err != nil {
			if stale > 0 {
				// The stamp inside says how long it has gone unrefreshed; its mtime is the fallback for a holder killed before it could write one.
				_, stamp, readErr := readLock(sc, lockPath)
				if readErr != nil {
					fi, statErr := sc.Stat(lockPath)
					if statErr == nil {
						stamp = fi.ModTime()
					}
				}

				// Reap it and let the retry race for it - two reapers racing is benign, exactly one O_EXCL create wins.
				if !stamp.IsZero() && time.Since(stamp) > stale {
					_ = sc.Remove(lockPath)
				}
			}

			return err
		}

		_, err = f.Write(fmt.Appendf(nil, "%s\n%019d", owner, time.Now().UnixNano()))
		r.client.WarnError(f.Close, "Failed to close a sFTP file")
		if err != nil {
			// The create already won the race, so this holder is the only one that can take back the file it is about to abandon.
			_ = sc.Remove(lockPath)
		}

		return err
	})
	if err != nil {
		return nil, ErrOperation.WithText("acquiring lock " + name).Wrap(err)
	}

	lock := &VolumeLock{
		client: r.client,
		sc:     sc,
		path:   lockPath,
		owner:  owner,
	}

	if stale > 0 {
		hbCtx, cancel := context.WithCancel(ctx)
		lock.cancel = cancel
		lock.wg.Add(1)
		go lock.heartbeat(hbCtx, stale)
	}

	return lock, nil
}

// readLock answers who holds the lock file and when they last wrote to it.
func readLock(sc *sftp.Client, path string) (string, time.Time, error) {
	f, err := sc.Open(path)
	if err != nil {
		return "", time.Time{}, err
	}

	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(f)
	if err != nil {
		return "", time.Time{}, err
	}

	owner, stamp, ok := strings.Cut(string(data), "\n")
	if !ok {
		return "", time.Time{}, fmt.Errorf("lock file %s carries no timestamp", path)
	}

	nanos, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("lock file %s carries an unreadable timestamp: %w", path, err)
	}

	return owner, time.Unix(0, nanos), nil
}

// heartbeat periodically rewrites the lock file so other holders don't consider it stale.
func (l *VolumeLock) heartbeat(ctx context.Context, stale time.Duration) {
	defer l.wg.Done()

	ticker := time.NewTicker(stale / 3)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			err := l.refresh(now)
			if err != nil {
				l.client.LogWarn("failed to refresh volume lock", "path", l.path, "error", err)
			}
		}
	}
}

// refresh writes t into the lock file, in place so no reader sees it empty, and
// truncates in case a record ever comes out shorter than the one it replaces.
func (l *VolumeLock) refresh(t time.Time) error {
	f, err := l.sc.OpenFile(l.path, os.O_WRONLY)
	if err != nil {
		return err
	}

	defer l.client.WarnError(f.Close, "Failed to close a sFTP file")

	data := fmt.Appendf(nil, "%s\n%019d", l.owner, t.UnixNano())

	_, err = f.Write(data)
	if err != nil {
		return err
	}

	return f.Truncate(int64(len(data)))
}

// Unlock releases the lock, deleting the lock file only if it still names this holder as owner.
func (l *VolumeLock) Unlock() error {
	if l.cancel != nil {
		l.cancel()
		l.wg.Wait()
	}

	owner, _, err := readLock(l.sc, l.path)
	if err != nil {
		l.client.LogWarn("volume lock file unreadable on unlock", "path", l.path, "error", err)
		return nil
	}

	if owner != l.owner {
		return nil
	}

	return l.sc.Remove(l.path)
}
