package keyed_test

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sirrobot01/decypharr/internal/task/keyed"
)

// recoveredText runs use and returns what it panicked with, as text.
func recoveredText(use func()) string {
	var text string
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				text, _ = recovered.(string)
			}
		}()
		use()
	}()
	return text
}

// Effects belongs to the Handle call it was passed to. Keeping it for later is a silent race, so every method
// panics once Handle has returned.
func TestEffectsPanicWhenUsedAfterHandleReturned(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		kept := make(chan *keyed.Effects[string, msg], 1)
		rec := newRecorder()
		r := keyed.New[string, msg, struct{}](newRoot(t), "kept", rec, keyed.Options[msg]{})
		mustSend(t, r, "a", msg{id: 1, run: func(_ context.Context, fx *keyed.Effects[string, msg]) { kept <- fx }})
		synctest.Wait()
		fx := <-kept

		for name, use := range map[string]func(){
			"Go":          func() { _, _ = fx.Go("late", func(context.Context) msg { return msg{} }, nil) },
			"CancelChild": func() { fx.CancelChild(1) },
			"Send":        func() { _ = fx.Send("b", msg{}) },
			"After":       func() { fx.After(time.Second, msg{}) },
			"CancelTimer": func() { fx.CancelTimer(1) },
			"Reply":       func() { fx.Reply(1) },
		} {
			if text := recoveredText(use); !strings.Contains(text, "after Handle returned") {
				t.Errorf("%s after Handle returned panicked with %q, want a panic saying so", name, text)
			}
		}
	})
}
