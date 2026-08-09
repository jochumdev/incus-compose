package client

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"sync"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/pkg/sftp"
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

	return conn.GetStoragePoolVolumeFileSFTP(ctx, r.Config.Pool, "custom", r.incusName)
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
	owner := fmt.Sprintf("%s:%d:%s", hostname, os.Getpid(), RandString(8))

	err := retry.New(
		retry.Context(ctx),
		retry.Attempts(0),
		retry.Delay(250*time.Millisecond),
		retry.DelayType(retry.FixedDelay),
		retry.LastErrorOnly(true),
	).Do(func() error {
		f, err := sc.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
		if err != nil {
			// Another holder has it; if stale, reap it and let the retry race for it - two reapers racing is benign, exactly one O_EXCL create wins.
			if stale > 0 {
				fi, statErr := sc.Stat(lockPath)
				if statErr == nil && time.Since(fi.ModTime()) > stale {
					_ = sc.Remove(lockPath)
				}
			}
			return err
		}
		defer r.client.WarnError(f.Close, "Failed to close a sFTP file")

		_, err = f.Write([]byte(owner))
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

// heartbeat periodically touches the lock file's mtime so other holders don't consider it stale.
func (l *VolumeLock) heartbeat(ctx context.Context, stale time.Duration) {
	defer l.wg.Done()

	ticker := time.NewTicker(stale / 3)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// l.client.LogWarn("volume lock heartbeat stopped", "path", l.path)
			return
		case now := <-ticker.C:
			if err := l.sc.Chtimes(l.path, now, now); err != nil {
				l.client.LogWarn("failed to refresh volume lock", "path", l.path, "error", err)
			}
		}
	}
}

// Unlock releases the lock, deleting the lock file only if it still names this holder as owner -
// a stale takeover may have replaced it, and deleting unconditionally would delete the new holder's lock instead.
// It does not close sc; the caller that passed it to Lock owns it.
func (l *VolumeLock) Unlock() error {
	if l.cancel != nil {
		l.cancel()
		l.wg.Wait()
	}

	f, err := l.sc.Open(l.path)
	if err != nil {
		l.client.LogWarn("volume lock file missing on unlock", "path", l.path, "error", err)
		return nil
	}

	data, readErr := io.ReadAll(f)
	l.client.WarnError(f.Close, "Failed to close a sFTP file")
	if readErr != nil {
		return readErr
	}

	if string(data) != l.owner {
		return nil
	}

	return l.sc.Remove(l.path)
}
