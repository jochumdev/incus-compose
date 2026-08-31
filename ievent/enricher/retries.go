package enricher

import (
	"iter"
	"slices"
	"time"
)

// retry is one key due another read, and when.
type retry struct {
	subject

	at time.Time
}

// retries is every key due another read, oldest first, with what each has
// already spent. Owned by the goroutine Run owns.
type retries struct {
	due []retry

	// tries is what each key has spent, so one that never settles ends up with
	// the run rather than being read for ever.
	tries map[string]int

	// timer fires when the head of due falls due.
	timer *time.Timer
}

// newRetries prepares the schedule, with nothing due.
func newRetries() *retries {
	timer := time.NewTimer(retryDelay)
	timer.Stop()

	return &retries{tries: map[string]int{}, timer: timer}
}

// done clears what one key has spent, so a key that comes back starts again
// from retryTries.
func (r *retries) done(project, name string) {
	delete(r.tries, resourceKey(kindInstance, project, name))
}

// stop disarms the timer.
func (r *retries) stop() { r.timer.Stop() }

// take is every key that has fallen due by now, and re-arms for the next.
func (r *retries) take(now time.Time) iter.Seq[subject] {
	return func(yield func(subject) bool) {
		for len(r.due) > 0 && !r.due[0].at.After(now) {
			if !yield(r.due[0].subject) {
				return
			}
			r.due = r.due[1:]
		}

		if len(r.due) > 0 {
			r.timer.Reset(r.due[0].at.Sub(now))
		}
	}
}

// soon schedules another read of one key after a read that did not settle,
// never inline (the events queued after it would wait it out), slowing to
// slowRetryDelay rather than stopping once it spends its fast attempts.
func (r *retries) soon(project, name string) {
	key := resourceKey(kindInstance, project, name)

	delay := retryDelay
	if r.tries[key] >= retryTries {
		delay = slowRetryDelay
	}

	r.tries[key]++

	r.at(project, name, delay)
}

// at schedules one key for a read in d. A key already due is left where it is:
// the earlier time is the one that keeps the promise, and a second read of one
// key joins the call the first sent anyway.
func (r *retries) at(project, name string, d time.Duration) {
	at := time.Now().Add(d)

	// Ordered by when, not appended: the timer is armed for the head, and a
	// fast re-read after a slow one would otherwise wait it out.
	i := len(r.due)
	for i > 0 && r.due[i-1].at.After(at) {
		i--
	}

	r.due = slices.Insert(r.due, i, retry{
		subject: subject{project: project, instance: name},
		at:      at,
	})

	if i == 0 {
		r.timer.Reset(d)
	}
}
