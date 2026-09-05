package player

import (
	"strconv"
	"time"

	"iar/internal/session"
)

// Changing the sound used to overwrite the session that was playing:
// steer twice and the vibe you liked was gone, with nothing to go back
// to but your memory of what you typed. So a change branches instead.
// The state before the change keeps its name and stays on disk; the
// changed session - the one now playing - carries on under a new
// generated name that says where it came from. Going back is then just
// loading the old name, and trying the same change again from there is
// one more branch rather than a fight with the last one.
//
// The branch is skipped while nothing has been heard from the session
// yet, because a state nobody has heard is not a state anyone wants
// back: three steers in a row while the first track is still rendering
// leave one session, not three.

// forkLocked branches the playing session. Callers hold o.mu, have
// already applied their change to o.sess, and pass the snapshot they
// took just before applying it. It reports whether a branch happened.
func (o *Orchestrator) forkLocked(before *session.Session) bool {
	if !o.heard || before == nil {
		return false
	}
	now := time.Now()
	name := session.ForkName(before.Name, now)
	// Two changes inside one second would land on the same name. The
	// next second along is still a generated name of the right shape;
	// a counter tacked on the end would not be, and the session would
	// quietly count as one the listener named.
	for n := 1; n < 120 && (o.store.Exists(name) || name == before.Name); n++ {
		name = session.ForkName(before.Name, now.Add(time.Duration(n)*time.Second))
	}
	o.sess.Name = name
	o.sess.Named = false
	o.sess.Created = now
	o.sess.ForkedFrom = before.Name
	// The branch has not been heard either, until it plays something.
	o.heard = false
	return true
}

// afterFork finishes a branch outside the lock: the state before the
// change goes back to disk under its own name, and the state directory
// names the new session so a restart resumes what is playing rather
// than what was. The returned note tells the listener where to find
// what they had, which is the whole point of branching.
func (o *Orchestrator) afterFork(before *session.Session, forked bool) string {
	if !forked {
		return ""
	}
	if err := o.store.Save(before); err != nil {
		o.log.Error("could not keep the previous session", "event", "session_fork_failed",
			"session", before.Name, "error", err.Error())
		return ""
	}
	o.recordCurrent()
	o.log.Info("session branched", "event", "session_forked",
		"from", before.Name, "to", o.CurrentName())
	return "; the sound before this is kept as " + before.Name
}

// DeleteAutoSessions clears out sessions that were never given a name -
// all of them, or only those not played for olderThanDays. The playing
// session is always kept, whatever its name.
func (o *Orchestrator) DeleteAutoSessions(olderThanDays int) string {
	var window time.Duration
	if olderThanDays > 0 {
		window = time.Duration(olderThanDays) * 24 * time.Hour
	}
	removed, err := o.store.DeleteAuto(time.Now(), window, o.CurrentName())
	if err != nil {
		return "could not delete the automatic sessions: " + err.Error()
	}
	o.log.Info("automatic sessions deleted", "event", "sessions_auto_deleted",
		"count", len(removed), "older_than_days", olderThanDays)
	if len(removed) == 0 {
		if olderThanDays > 0 {
			return "no automatic sessions older than " + strconv.Itoa(olderThanDays) + " day(s)"
		}
		return "there are no automatic sessions to delete"
	}
	what := "automatic sessions"
	if len(removed) == 1 {
		what = "automatic session"
	}
	ack := "deleted " + strconv.Itoa(len(removed)) + " " + what
	if olderThanDays > 0 {
		ack += " older than " + strconv.Itoa(olderThanDays) + " day(s)"
	}
	return ack
}
