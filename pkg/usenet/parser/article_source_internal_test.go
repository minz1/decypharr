package parser

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
)

type fakeArticleBackend struct {
	fetches   atomic.Int64
	stats     atomic.Int64
	active    atomic.Int64
	maxActive atomic.Int64
	started   chan struct{}
	release   chan struct{}
	bodySize  int

	statErr     error
	statRelease chan struct{}
}

func (b *fakeArticleBackend) Fetch(
	ctx context.Context,
	messageID string,
) ([]byte, *nntp.YencMetadata, nntp.Answer, error) {
	b.fetches.Add(1)
	active := b.active.Add(1)
	defer b.active.Add(-1)
	for {
		maximum := b.maxActive.Load()
		if active <= maximum || b.maxActive.CompareAndSwap(maximum, active) {
			break
		}
	}
	if b.started != nil {
		select {
		case b.started <- struct{}{}:
		default:
		}
	}
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
			return nil, nil, nntp.AnswerFailed, ctx.Err()
		}
	}
	size := b.bodySize
	if size == 0 {
		size = 16
	}
	body := make([]byte, size)
	copy(body, messageID)
	return body, &nntp.YencMetadata{Name: messageID, Size: int64(size), PartSize: int64(size)}, nntp.AnswerFound, nil
}

func (b *fakeArticleBackend) Stat(ctx context.Context, _ string) (nntp.Answer, error) {
	b.stats.Add(1)
	if b.statRelease != nil {
		select {
		case <-b.statRelease:
		case <-ctx.Done():
			return nntp.AnswerFailed, ctx.Err()
		}
	}
	return answerFromError(b.statErr), b.statErr
}

func TestArticleBrokerDeduplicatesConcurrentBodyAndHeader(t *testing.T) {
	t.Parallel()

	backend := &fakeArticleBackend{started: make(chan struct{}, 1), release: make(chan struct{})}
	broker := newArticleBroker(backend, 4, 1<<20)

	var wg sync.WaitGroup
	errors := make(chan error, 12)
	wg.Go(func() {
		_, err := broker.Body(t.Context(), "same@example")
		errors <- err
	})
	<-backend.started
	for index := range 11 {
		wg.Go(func() {
			if index%2 == 0 {
				_, _, err := broker.Header(t.Context(), "same@example", 8)
				errors <- err
				return
			}
			_, err := broker.Body(t.Context(), "same@example")
			errors <- err
		})
	}
	close(backend.release)
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := backend.fetches.Load(); got != 1 {
		t.Fatalf("network fetches = %d, want 1", got)
	}
	if _, err := broker.Stat(t.Context(), "same@example"); err != nil {
		t.Fatal(err)
	}
	if got := backend.stats.Load(); got != 0 {
		t.Fatalf("network stats = %d, want 0", got)
	}
}

func TestArticleBrokerBoundsNetworkConcurrency(t *testing.T) {
	t.Parallel()

	backend := &fakeArticleBackend{release: make(chan struct{})}
	broker := newArticleBroker(backend, 3, 1<<20)

	var wg sync.WaitGroup
	for index := range 12 {
		wg.Go(func() {
			_, _ = broker.Body(t.Context(), fmt.Sprintf("article-%d@example", index))
		})
	}
	deadline := time.Now().Add(time.Second)
	for backend.maxActive.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := backend.maxActive.Load(); got != 3 {
		close(backend.release)
		wg.Wait()
		t.Fatalf("maximum network concurrency = %d, want 3", got)
	}
	close(backend.release)
	wg.Wait()
	if got := backend.maxActive.Load(); got != 3 {
		t.Fatalf("maximum network concurrency = %d, want 3", got)
	}
}

func TestArticleBrokerRetainsMetadataAfterBodyEviction(t *testing.T) {
	t.Parallel()

	backend := &fakeArticleBackend{bodySize: 4}
	broker := newArticleBroker(backend, 1, 4)
	if _, err := broker.Body(t.Context(), "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Body(t.Context(), "two"); err != nil {
		t.Fatal(err)
	}
	if got := backend.fetches.Load(); got != 2 {
		t.Fatalf("network fetches = %d, want 2", got)
	}
	header, _, err := broker.Header(t.Context(), "one", 4)
	if err != nil {
		t.Fatal(err)
	}
	if string(header.Snippet) != "one\x00" {
		t.Fatalf("cached snippet = %q", header.Snippet)
	}
	if got := backend.fetches.Load(); got != 2 {
		t.Fatalf("header caused network refetch: %d", got)
	}
	if _, bodyErr := broker.Body(t.Context(), "one"); bodyErr != nil {
		t.Fatal(bodyErr)
	}
	if got := backend.fetches.Load(); got != 3 {
		t.Fatalf("evicted body fetches = %d, want 3", got)
	}
}

func TestArticleBrokerBoundsMetadataEntries(t *testing.T) {
	t.Parallel()

	backend := &fakeArticleBackend{bodySize: 4}
	broker := newArticleBroker(backend, 1, 1<<20)
	broker.entryLimit = 2
	for _, messageID := range []string{"one", "two"} {
		if _, err := broker.Body(t.Context(), messageID); err != nil {
			t.Fatal(err)
		}
	}
	if !broker.IsAvailable("one") {
		t.Fatal("first entry was not recorded")
	}
	if _, err := broker.Body(t.Context(), "three"); err != nil {
		t.Fatal(err)
	}
	if broker.IsAvailable("two") {
		t.Fatal("least-recently-used metadata entry was not evicted")
	}
	if !broker.IsAvailable("one") || !broker.IsAvailable("three") {
		t.Fatal("recent metadata entries were evicted")
	}
	metrics := broker.Metrics()
	if metrics.CachedEntries != 2 || metrics.CachedBodies != 2 {
		t.Fatalf("bounded cache metrics = %+v", metrics)
	}
}

// Message ids decoded from a v2 .meta blob are views into one large
// decompressed buffer, so a retained id pins that NZB's whole segment map. The
// broker outlives any single parse, so every key it keeps must be its own copy.
func TestArticleBrokerDoesNotRetainAliasedMessageIDs(t *testing.T) {
	t.Parallel()
	const id = "aliased-segment@example"
	backing := make([]byte, 1<<20)
	for index := range backing {
		backing[index] = 'x'
	}
	copy(backing[4096:], id)
	aliased := unsafe.String(&backing[4096], len(id))
	if aliased != id {
		t.Fatalf("aliased id = %q", aliased)
	}

	broker := newArticleBroker(&fakeArticleBackend{bodySize: 8}, 1, 1<<20)
	if _, err := broker.Body(t.Context(), aliased); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Stat(t.Context(), aliased); err != nil {
		t.Fatal(err)
	}

	low := uintptr(unsafe.Pointer(&backing[0]))
	high := low + uintptr(len(backing))
	aliases := func(value string) bool {
		if value == "" {
			return false
		}
		pointer := uintptr(unsafe.Pointer(unsafe.StringData(value)))
		return pointer >= low && pointer < high
	}

	broker.mu.Lock()
	defer broker.mu.Unlock()
	if len(broker.entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(broker.entries))
	}
	for key := range broker.entries {
		if aliases(key) {
			t.Fatalf("entries key %q aliases the caller's buffer", key)
		}
	}
	for element := broker.entryLRU.Front(); element != nil; element = element.Next() {
		if aliases(element.Value.(string)) {
			t.Fatal("entry LRU retains a key aliasing the caller's buffer")
		}
	}
	for element := broker.lru.Front(); element != nil; element = element.Next() {
		if aliases(element.Value.(string)) {
			t.Fatal("body LRU retains a key aliasing the caller's buffer")
		}
	}
}

// A caller that gives up must not fail the others waiting on the same
// article: the shared fetch ran on the first caller's context.
func TestBrokerFetchSurvivesFirstCallerCancel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		backend := &fakeArticleBackend{release: make(chan struct{}), bodySize: 16}
		broker := newArticleBroker(backend, 2, 1<<20)

		firstCtx, cancelFirst := context.WithCancel(t.Context())
		firstErr := make(chan error, 1)
		go func() {
			_, err := broker.load(firstCtx, "<article>")
			firstErr <- err
		}()
		synctest.Wait()
		secondErr := make(chan error, 1)
		go func() {
			_, err := broker.load(t.Context(), "<article>")
			secondErr <- err
		}()
		synctest.Wait()

		cancelFirst()
		if err := <-firstErr; !errors.Is(err, context.Canceled) {
			t.Fatalf("first caller err = %v", err)
		}
		close(backend.release)
		if err := <-secondErr; err != nil {
			t.Fatalf("second caller failed with the first caller's cancellation: %v", err)
		}
		if got := backend.fetches.Load(); got != 1 {
			t.Fatalf("fetches = %d, want one shared fetch", got)
		}
	})
}

func TestNNTPBackendFetchProvesMissing(t *testing.T) {
	t.Parallel()
	server, err := nntpd.New(nntpd.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	client := wireClient(t, server)
	_, _, _, err = nntpArticleBackend{manager: client}.Fetch(t.Context(), "missing@example")
	if !nntp.IsNotFoundEverywhere(err) {
		t.Fatalf("err = %v", err)
	}
}

func wireClient(t *testing.T, server *nntpd.Server) *nntp.Client {
	t.Helper()
	root := task.NewRoot(t.Context(), "test", zerolog.Nop())
	t.Cleanup(func() {
		root.Close()
		<-root.Done()
	})
	host, port := server.Addr()
	client, err := nntp.NewClient(root.Child("nntp"), &config.Config{Usenet: config.Usenet{
		Providers: []config.UsenetProvider{{Host: host, Port: port, MaxConnections: 1}},
	}}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestBrokerHeaderAnswersFromWire(t *testing.T) {
	t.Parallel()
	server, err := nntpd.New(nntpd.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	payload := nntpd.Pattern(0, 64)
	server.AddArticle("<served@example>", nntpd.Encode(payload, "a.bin", 1, 64, 0))
	broker := NewArticleBroker(wireClient(t, server), 2, 1<<20)

	if _, answer, headerErr := broker.Header(
		t.Context(),
		"absent@example",
		0,
	); headerErr == nil ||
		answer != nntp.AnswerMissing {
		t.Fatalf("absent: answer = %v err = %v, want missing", answer, headerErr)
	}
	if _, answer, headerErr := broker.Header(
		t.Context(),
		"served@example",
		0,
	); headerErr != nil ||
		answer != nntp.AnswerFound {
		t.Fatalf("served: answer = %v err = %v, want found", answer, headerErr)
	}
	if _, answer, headerErr := broker.Header(
		t.Context(),
		"served@example",
		0,
	); headerErr != nil ||
		answer != nntp.AnswerFound {
		t.Fatalf("cached: answer = %v err = %v, want found", answer, headerErr)
	}
	if got := broker.Metrics().CacheHits; got == 0 {
		t.Fatal("third call did not hit the cache")
	}
}

func TestBrokerFlightSharesAnswer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		backend := &fakeArticleBackend{statErr: bare430(), statRelease: make(chan struct{})}
		broker := newArticleBroker(backend, 2, 1<<20)

		type result struct {
			answer nntp.Answer
			err    error
		}
		results := make(chan result, 2)
		for range 2 {
			go func() {
				answer, err := broker.Stat(t.Context(), "<article>")
				results <- result{answer, err}
			}()
		}
		synctest.Wait()
		close(backend.statRelease)
		for range 2 {
			got := <-results
			if got.answer != nntp.AnswerNotFound || got.err == nil {
				t.Fatalf("answer = %v err = %v, want not_found with error", got.answer, got.err)
			}
		}
		if got := backend.stats.Load(); got != 1 {
			t.Fatalf("network stats = %d, want one shared flight", got)
		}
	})
}

func TestBrokerCancelledWaiterIsFailed(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		backend := &fakeArticleBackend{statRelease: make(chan struct{})}
		broker := newArticleBroker(backend, 2, 1<<20)

		ctx, cancel := context.WithCancel(t.Context())
		type result struct {
			answer nntp.Answer
			err    error
		}
		done := make(chan result, 1)
		go func() {
			answer, err := broker.Stat(ctx, "<article>")
			done <- result{answer, err}
		}()
		synctest.Wait()
		cancel()
		got := <-done
		if got.answer != nntp.AnswerFailed || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("answer = %v err = %v, want failed with cancellation", got.answer, got.err)
		}
		close(backend.statRelease)
		synctest.Wait()
	})
}

func TestAnswerFromErrorOrder(t *testing.T) {
	t.Parallel()
	crc := &nntp.Error{Type: nntp.ErrorTypeYencDecode, Message: "crc mismatch"}
	cases := []struct {
		name string
		err  error
		want nntp.Answer
	}{
		{"nil", nil, nntp.AnswerFound},
		{"bare 430", bare430(), nntp.AnswerNotFound},
		{"proven 430", proven430(), nntp.AnswerMissing},
		{"marker around crc", &nntp.NotFoundEverywhereError{Err: crc}, nntp.AnswerCorrupt},
		{"crc", crc, nntp.AnswerCorrupt},
		{"other", errors.New("boom"), nntp.AnswerFailed},
	}
	for _, tc := range cases {
		if got := answerFromError(tc.err); got != tc.want {
			t.Errorf("%s: answer = %v, want %v", tc.name, got, tc.want)
		}
	}
}
