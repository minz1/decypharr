package usenet

import (
	"context"
	"testing"
	"testing/synctest"
)

// A stop whose deadline has already passed cancels every task at once, so
// finalize starts while an entry creator may still hold its work lease. The
// entry that creator stores must still be torn down by finalize: once the
// service group is done, the map is empty.
func TestRushedStopCleansAnEntryStoredUnderALease(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		u, root := newLifecycleUsenet(t, 1, 1) // nothing is dialled: the entry is never read
		addLifecycleFile(t, u, "nzb", "movie.mkv", 2)
		file, err := u.getFile("nzb", "movie.mkv")
		if err != nil {
			t.Fatal(err)
		}

		// A creator inside getOrCreateEntry's slow path: it holds the lease
		// and has not stored its entry yet.
		leave, err := u.work.Enter("entry")
		if err != nil {
			t.Fatal(err)
		}
		entry, err := u.buildEntry(file, u.prefetchSize, RetentionWindow)
		if err != nil {
			t.Fatal(err)
		}

		expired, cancel := context.WithCancel(t.Context())
		cancel()
		_ = root.Stop(expired) // rushed: reports the lease as still running
		synctest.Wait()        // finalize has run as far as it can

		stored, err := u.storeEntry(t.Context(), entryKey("nzb", "movie.mkv", RetentionWindow), entry)
		if err != nil {
			t.Fatal(err)
		}
		stored.releaseUsed()
		leave()

		<-root.Done()
		if n := u.fs.Size(); n != 0 {
			t.Fatalf("%d entries left in the map after a rushed stop", n)
		}
	})
}
