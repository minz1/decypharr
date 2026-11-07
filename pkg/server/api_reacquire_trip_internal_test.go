package server

import (
	"fmt"
	"net/http"
	"testing"
)

// A manual reacquisition is no evidence that anything is failing: any number of them in an hour never trip the
// breaker, even though each counts toward the caps.
func TestManualReacquiresThroughTheHandlerNeverTripTheBreaker(t *testing.T) {
	t.Parallel()
	s, post, bind := reacquireHandler(t)
	for n := range 12 {
		if n > 0 {
			bind(n)
		}
		body := fmt.Sprintf(`{"entryId":"entry-%d","fileId":"file","cause":"manual","strategy":"command_search"}`, n+1)
		if code, id := post(body); code != http.StatusAccepted || id == "" {
			t.Fatalf("manual reacquisition %d = %d (job %q), want accepted with a job", n, code, id)
		}
	}
	if status := s.manager.Repair().BreakerStatus(); len(status) != 0 {
		t.Fatalf("12 manual reacquires tripped the breaker: %+v", status)
	}
}
