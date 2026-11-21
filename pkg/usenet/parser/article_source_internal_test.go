package parser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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

	fetchErr   error
	noMetadata bool
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
	if b.fetchErr != nil {
		return nil, nil, answerFromError(b.fetchErr), b.fetchErr
	}
	size := b.bodySize
	if size == 0 {
		size = 16
	}
	body := make([]byte, size)
	copy(body, messageID)
	if b.noMetadata {
		return body, nil, nntp.AnswerFound, nil
	}
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
		type loaded struct {
			observation articleObservation
			err         error
		}
		first := make(chan loaded, 1)
		go func() {
			observation, err := broker.load(firstCtx, "<article>")
			first <- loaded{observation, err}
		}()
		synctest.Wait()
		second := make(chan loaded, 1)
		go func() {
			observation, err := broker.load(t.Context(), "<article>")
			second <- loaded{observation, err}
		}()
		synctest.Wait()

		cancelFirst()
		if got := <-first; !errors.Is(got.err, context.Canceled) || got.observation.answer != nntp.AnswerFailed {
			t.Fatalf("first caller answer = %v err = %v", got.observation.answer, got.err)
		}
		close(backend.release)
		if got := <-second; got.err != nil || got.observation.answer != nntp.AnswerFound {
			t.Fatalf("second caller answer = %v err = %v, want found despite the first caller's cancellation",
				got.observation.answer, got.err)
		}
		if got := backend.fetches.Load(); got != 1 {
			t.Fatalf("fetches = %d, want one shared fetch", got)
		}
	})
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
		{
			"marker around non-430",
			&nntp.NotFoundEverywhereError{Err: errors.New("connection reset")},
			nntp.AnswerFailed,
		},
	}
	for _, tc := range cases {
		if got := answerFromError(tc.err); got != tc.want {
			t.Errorf("%s: answer = %v, want %v", tc.name, got, tc.want)
		}
	}
}

type answered struct {
	answer nntp.Answer
	err    error
}

func TestStatWithKeepsAnswerAndError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want nntp.Answer
	}{
		{"nil", nil, nntp.AnswerFound},
		{"bare 430", bare430(), nntp.AnswerNotFound},
		{"proven 430", proven430(), nntp.AnswerMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			answer, err := statWith(func(context.Context, string) error { return tc.err })(t.Context(), "<id>")
			if answer != tc.want || !errors.Is(err, tc.err) {
				t.Fatalf("answer = %v err = %v, want %v and %v", answer, err, tc.want, tc.err)
			}
		})
	}
}

const servedArticle = "served@example"

func wireServing(t *testing.T, cfg nntpd.Config, article []byte) *nntp.Client {
	t.Helper()
	server, err := nntpd.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	server.AddArticle("<"+servedArticle+">", article)
	return wireClient(t, server)
}

func wireWithArticle(t *testing.T) *nntp.Client {
	t.Helper()
	return wireServing(t, nntpd.Config{}, nntpd.Encode(nntpd.Pattern(0, 64), "a.bin", 1, 64, 0))
}

func corruptCRCArticle(t *testing.T) []byte {
	t.Helper()
	body := nntpd.Encode(nntpd.Pattern(0, 64), "a.bin", 1, 64, 0)
	marker := []byte("pcrc32=")
	at := bytes.Index(body, marker)
	if at < 0 {
		t.Fatal("encoded body has no pcrc32")
	}
	copy(body[at+len(marker):], "ffffffff")
	return body
}

func replying(command, line string) func(string, string) string {
	return func(got, _ string) string {
		if got == command {
			return line
		}
		return ""
	}
}

func headerAnswer(ctx context.Context, b *ArticleBroker, id string, snippet int) answered {
	_, answer, err := b.Header(ctx, id, snippet)
	return answered{answer, err}
}

func statAnswer(ctx context.Context, b *ArticleBroker) answered {
	answer, err := b.Stat(ctx, "<article>")
	return answered{answer, err}
}

func requireBody(t *testing.T, b *ArticleBroker, id string) {
	t.Helper()
	if _, err := b.Body(t.Context(), id); err != nil {
		t.Fatal(err)
	}
}

func requireTraffic(t *testing.T, backend *fakeArticleBackend, broker *ArticleBroker, fetches, stats, hits int64) {
	t.Helper()
	got := [3]int64{backend.fetches.Load(), backend.stats.Load(), broker.Metrics().CacheHits}
	if got != [3]int64{fetches, stats, hits} {
		t.Fatalf("network fetches, stats, cache hits = %v, want %v", got, [3]int64{fetches, stats, hits})
	}
}

func sharedWaiters(t *testing.T, release chan struct{}, call func(context.Context) answered) answered {
	t.Helper()
	results := make(chan answered, 2)
	for range 2 {
		go func() { results <- call(t.Context()) }()
	}
	synctest.Wait()
	close(release)
	first, second := <-results, <-results
	if first.answer != second.answer || !errors.Is(first.err, second.err) || !errors.Is(second.err, first.err) {
		t.Fatalf("waiters disagree: %+v vs %+v", first, second)
	}
	return first
}

func cancelledWaiter(t *testing.T, release chan struct{}, call func(context.Context) answered) answered {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan answered, 1)
	go func() { done <- call(ctx) }()
	synctest.Wait()
	cancel()
	got := <-done
	close(release)
	synctest.Wait()
	return got
}

func inBubble(t *testing.T, run func(t *testing.T) answered) answered {
	var got answered
	synctest.Test(t, func(t *testing.T) { got = run(t) })
	return got
}

func isEmptyID(err error) bool {
	return err != nil && strings.Contains(err.Error(), "article message ID is empty")
}

func isCanceled(err error) bool { return errors.Is(err, context.Canceled) }

func isNoMetadata(err error) bool {
	return err != nil && strings.Contains(err.Error(), "returned no yEnc metadata")
}

type answerRow struct {
	name    string
	want    nntp.Answer
	wantErr func(error) bool
	run     func(t *testing.T) answered
}

func wireRows() []answerRow {
	const absent = "absent@example"
	return []answerRow{
		{"wire stat absent", nntp.AnswerMissing, nntp.IsNotFoundEverywhere, func(t *testing.T) answered {
			answer, err := nntpArticleBackend{manager: wireWithArticle(t)}.Stat(t.Context(), absent)
			return answered{answer, err}
		}},
		{"wire stat served", nntp.AnswerFound, nil, func(t *testing.T) answered {
			answer, err := nntpArticleBackend{manager: wireWithArticle(t)}.Stat(t.Context(), servedArticle)
			return answered{answer, err}
		}},
		{"wire fetch absent", nntp.AnswerMissing, nntp.IsNotFoundEverywhere, func(t *testing.T) answered {
			_, _, answer, err := nntpArticleBackend{manager: wireWithArticle(t)}.Fetch(t.Context(), absent)
			return answered{answer, err}
		}},
		{"wire fetch served", nntp.AnswerFound, nil, func(t *testing.T) answered {
			_, _, answer, err := nntpArticleBackend{manager: wireWithArticle(t)}.Fetch(t.Context(), servedArticle)
			return answered{answer, err}
		}},
		{"wire body absent", nntp.AnswerMissing, nntp.IsNotFoundEverywhere, func(t *testing.T) answered {
			return headerAnswer(t.Context(), NewArticleBroker(wireWithArticle(t), 2, 1<<20), absent, 0)
		}},
		{"wire body served", nntp.AnswerFound, nil, func(t *testing.T) answered {
			return headerAnswer(t.Context(), NewArticleBroker(wireWithArticle(t), 2, 1<<20), servedArticle, 0)
		}},
		{"wire stat 423", nntp.AnswerNotFound, nntp.IsArticleNotFoundError, func(t *testing.T) answered {
			client := wireServing(t, nntpd.Config{Reply: replying("STAT", "423 no number")}, nil)
			answer, err := NewArticleBroker(client, 2, 1<<20).Stat(t.Context(), servedArticle)
			return answered{answer, err}
		}},
		{"wire corrupt body", nntp.AnswerCorrupt, nntp.IsYencDecodeError, func(t *testing.T) answered {
			client := wireServing(t, nntpd.Config{}, corruptCRCArticle(t))
			return headerAnswer(t.Context(), NewArticleBroker(client, 2, 1<<20), servedArticle, 0)
		}},
		{"wire metadata-only cache hit", nntp.AnswerFound, nil, func(t *testing.T) answered {
			broker := NewArticleBroker(wireWithArticle(t), 2, 1<<20)
			if got := headerAnswer(t.Context(), broker, servedArticle, 0); got.answer != nntp.AnswerFound || got.err != nil {
				t.Fatalf("first header = %+v", got)
			}
			got := headerAnswer(t.Context(), broker, servedArticle, 0)
			if hits := broker.Metrics().CacheHits; hits != 1 {
				t.Fatalf("cache hits = %d, want 1", hits)
			}
			return got
		}},
	}
}

func cacheRows() []answerRow {
	return []answerRow{
		{"stat cache hit", nntp.AnswerFound, nil, func(t *testing.T) answered {
			backend := &fakeArticleBackend{}
			broker := newArticleBroker(backend, 2, 1<<20)
			requireBody(t, broker, "<article>")
			got := statAnswer(t.Context(), broker)
			requireTraffic(t, backend, broker, 1, 0, 1)
			return got
		}},
		{"body cache hit through load", nntp.AnswerFound, nil, func(t *testing.T) answered {
			backend := &fakeArticleBackend{bodySize: 2 * maxCachedArticlePrefix}
			broker := newArticleBroker(backend, 2, 1<<20)
			requireBody(t, broker, "<article>")
			got := headerAnswer(t.Context(), broker, "<article>", maxCachedArticlePrefix+1)
			requireTraffic(t, backend, broker, 1, 0, 1)
			return got
		}},
		{"header prefix cache hit", nntp.AnswerFound, nil, func(t *testing.T) answered {
			backend := &fakeArticleBackend{}
			broker := newArticleBroker(backend, 2, 1<<20)
			requireBody(t, broker, "<article>")
			got := headerAnswer(t.Context(), broker, "<article>", 8)
			requireTraffic(t, backend, broker, 1, 0, 1)
			return got
		}},
		{"stat flight success", nntp.AnswerFound, nil, func(t *testing.T) answered {
			backend := &fakeArticleBackend{}
			broker := newArticleBroker(backend, 2, 1<<20)
			got := statAnswer(t.Context(), broker)
			requireTraffic(t, backend, broker, 0, 1, 0)
			return got
		}},
	}
}

func flightRows() []answerRow {
	return []answerRow{
		{"shared body waiters", nntp.AnswerMissing, nntp.IsNotFoundEverywhere, func(t *testing.T) answered {
			return inBubble(t, func(t *testing.T) answered {
				backend := &fakeArticleBackend{fetchErr: proven430(), release: make(chan struct{})}
				broker := newArticleBroker(backend, 2, 1<<20)
				got := sharedWaiters(t, backend.release, func(ctx context.Context) answered {
					return headerAnswer(ctx, broker, "<article>", 0)
				})
				requireTraffic(t, backend, broker, 1, 0, 0)
				return got
			})
		}},
		{"shared stat waiters", nntp.AnswerNotFound, nntp.IsArticleNotFoundError, func(t *testing.T) answered {
			return inBubble(t, func(t *testing.T) answered {
				backend := &fakeArticleBackend{statErr: bare430(), statRelease: make(chan struct{})}
				broker := newArticleBroker(backend, 2, 1<<20)
				got := sharedWaiters(t, backend.statRelease, func(ctx context.Context) answered {
					return statAnswer(ctx, broker)
				})
				requireTraffic(t, backend, broker, 0, 1, 0)
				return got
			})
		}},
		{"cancelled body waiter", nntp.AnswerFailed, isCanceled, func(t *testing.T) answered {
			return inBubble(t, func(t *testing.T) answered {
				backend := &fakeArticleBackend{release: make(chan struct{})}
				broker := newArticleBroker(backend, 2, 1<<20)
				return cancelledWaiter(t, backend.release, func(ctx context.Context) answered {
					return headerAnswer(ctx, broker, "<article>", 0)
				})
			})
		}},
		{"cancelled stat waiter", nntp.AnswerFailed, isCanceled, func(t *testing.T) answered {
			return inBubble(t, func(t *testing.T) answered {
				backend := &fakeArticleBackend{statRelease: make(chan struct{})}
				broker := newArticleBroker(backend, 2, 1<<20)
				return cancelledWaiter(t, backend.statRelease, func(ctx context.Context) answered {
					return statAnswer(ctx, broker)
				})
			})
		}},
		{"empty message id", nntp.AnswerFailed, isEmptyID, func(t *testing.T) answered {
			broker := newArticleBroker(&fakeArticleBackend{}, 2, 1<<20)
			return headerAnswer(t.Context(), broker, "", 0)
		}},
		{"fetch returns no metadata", nntp.AnswerFailed, isNoMetadata, func(t *testing.T) answered {
			broker := newArticleBroker(&fakeArticleBackend{noMetadata: true}, 2, 1<<20)
			return headerAnswer(t.Context(), broker, "<article>", 0)
		}},
	}
}

func TestEveryAnswerPath(t *testing.T) {
	t.Parallel()
	rows := slices.Concat(wireRows(), cacheRows(), flightRows())
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got := row.run(t)
			if got.answer != row.want {
				t.Errorf("answer = %v, want %v", got.answer, row.want)
			}
			if row.wantErr == nil && got.err != nil {
				t.Errorf("err = %v, want none", got.err)
			}
			if row.wantErr != nil && !row.wantErr(got.err) {
				t.Errorf("err = %v, not the expected error", got.err)
			}
		})
	}
}
