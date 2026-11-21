package nntp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
)

func TestFailoverAttributesMissingArticleToActualProvider(t *testing.T) {
	t.Parallel()
	first, err := nntpd.New(nntpd.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := nntpd.New(nntpd.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	host, firstPort := first.Addr()
	_, secondPort := second.Addr()
	providers := []config.UsenetProvider{
		{Host: host, Port: firstPort, MaxConnections: 1, Priority: 1, Backbone: "first"},
		{Host: "localhost", Port: secondPort, MaxConnections: 1, Priority: 2, Backbone: "second"},
	}
	cfg := &config.Config{Retries: 1, Usenet: config.Usenet{Providers: providers}}
	client, err := NewClient(newTestGroup(t), cfg, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	var visited []string
	err = client.ExecuteWithFailover(t.Context(), WorkloadStreamDemand, func(conn *Connection) error {
		backbone := conn.pool.config.Backbone
		visited = append(visited, backbone)
		if backbone == "second" {
			return &Error{Type: ErrorTypeArticleNotFound}
		}
		if len(visited) == 1 {
			return NewConnectionError(errors.New("connection failed"))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failover failed: %v", err)
	}
	if !slices.Equal(visited, []string{"first", "second", "first"}) {
		t.Fatalf("visited %v", visited)
	}
	for _, pp := range client.orderedPools {
		if len(pp.slots) != 0 {
			t.Fatalf("provider %s leaked an admission slot", pp.config.Host)
		}
	}
}

func TestFailoverHonorsSingleProviderRetryBudget(t *testing.T) {
	t.Parallel()
	server, err := nntpd.New(nntpd.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	host, port := server.Addr()
	client, err := NewClient(
		newTestGroup(t),
		&config.Config{
			Retries: 1,
			Usenet:  config.Usenet{Providers: []config.UsenetProvider{{Host: host, Port: port, MaxConnections: 1}}},
		},
		zerolog.Nop(),
	)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	failure := NewTimeoutError(errors.New("timeout"))
	err = client.ExecuteWithFailover(
		t.Context(),
		WorkloadStreamDemand,
		func(*Connection) error { calls++; return failure },
	)
	if calls != 2 || !errors.Is(err, ErrAllProvidersFailed) || !errors.Is(err, failure) {
		t.Fatalf("calls=%d, error=%v", calls, err)
	}
	if len(client.orderedPools[0].slots) != 0 {
		t.Fatal("retry budget exhaustion leaked an admission slot")
	}
}

func deadPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if closeErr := ln.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	return port
}

func scriptedReply(command, messageID string) string {
	if command != "STAT" && command != "BODY" {
		return ""
	}
	code, _, ok := strings.Cut(strings.Trim(messageID, "<>"), "@")
	if _, convErr := strconv.Atoi(code); !ok || convErr != nil || code == scriptBodyCode {
		return ""
	}
	return code + " scripted"
}

const scriptBodyCode = "222"

func livePort(t *testing.T) int {
	t.Helper()
	server, err := nntpd.New(nntpd.Config{Reply: scriptedReply})
	if err != nil {
		t.Fatal(err)
	}
	server.AddArticle("<"+scriptBodyCode+"@script>", []byte("no yenc data here\r\n"))
	t.Cleanup(server.Close)
	_, port := server.Addr()
	return port
}

func failoverProvider(
	host string, port int, user string, priority int, backbone string, backup bool,
) config.UsenetProvider {
	return config.UsenetProvider{
		Host: host, Port: port, Username: user, MaxConnections: 1,
		Priority: priority, Backbone: backbone, Backup: backup,
	}
}

func failoverClient(t *testing.T, retries int, providers ...config.UsenetProvider) *Client {
	t.Helper()
	client, err := NewClient(
		newTestGroup(t),
		&config.Config{Retries: retries, Usenet: config.Usenet{Providers: providers}},
		zerolog.Nop(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type scriptStep struct {
	reply string
	wrap  func(error) error
}

func (s scriptStep) run(conn *Connection) error {
	var err error
	switch s.reply {
	case "":
	case scriptBodyCode:
		_, err = conn.requestBody("<" + scriptBodyCode + "@script>")
	default:
		_, _, err = conn.Stat("<" + s.reply + "@script>")
	}
	if s.wrap != nil {
		return s.wrap(err)
	}
	return err
}

func bare430() error {
	return &Error{Type: ErrorTypeArticleNotFound, Code: 430, Message: "no such article"}
}

func notFound430() scriptStep {
	return scriptStep{reply: "430"}
}

func raised(err error) scriptStep {
	return scriptStep{wrap: func(error) error { return err }}
}

func wrapped430(wrap func(error) error) scriptStep {
	return scriptStep{reply: "430", wrap: wrap}
}

type scripted struct {
	mu     sync.Mutex
	script map[string][]scriptStep
	seen   []string
}

func (s *scripted) fn(conn *Connection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	user := conn.pool.config.Username
	s.seen = append(s.seen, user)
	queue := s.script[user]
	if len(queue) == 0 {
		return nil
	}
	step := queue[0]
	if len(queue) > 1 {
		s.script[user] = queue[1:]
	}
	return step.run(conn)
}

func runScripted(t *testing.T, client *Client, prove bool, script map[string][]scriptStep) ([]string, error) {
	t.Helper()
	s := &scripted{script: script}
	run := client.ExecuteWithFailover
	if prove {
		run = func(ctx context.Context, workload Workload, fn func(*Connection) error) error {
			_, err := client.ExecuteArticle(ctx, workload, fn)
			return err
		}
	}
	err := run(t.Context(), WorkloadDownload, s.fn)
	return s.seen, err
}

func twoHosts(t *testing.T, labelA, labelB string) *Client {
	t.Helper()
	port := livePort(t)
	return failoverClient(t, 0,
		failoverProvider("127.0.0.1", port, "a", 1, labelA, false),
		failoverProvider("localhost", port, "b", 2, labelB, false),
	)
}

func TestExecuteWithFailoverNeverProves(t *testing.T) {
	t.Parallel()
	client := twoHosts(t, "", "")
	_, err := runScripted(t, client, false, map[string][]scriptStep{"a": {notFound430()}, "b": {notFound430()}})
	if err == nil || IsNotFoundEverywhere(err) || !IsArticleNotFoundError(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestFailoverEveryProvider430IsNotFoundEverywhere(t *testing.T) {
	t.Parallel()
	script := func() map[string][]scriptStep {
		return map[string][]scriptStep{"a": {notFound430()}, "b": {notFound430()}}
	}
	_, plain := runScripted(t, twoHosts(t, "", ""), false, script())
	_, proven := runScripted(t, twoHosts(t, "", ""), true, script())
	if !IsNotFoundEverywhere(proven) || !IsArticleNotFoundError(proven) || !errors.Is(proven, ErrAllProvidersFailed) {
		t.Fatalf("err = %v", proven)
	}
	if proven.Error() != plain.Error() {
		t.Fatalf("text changed: %q vs %q", proven.Error(), plain.Error())
	}
}

func TestFailoverSharedBackbone430CoversTheLabel(t *testing.T) {
	t.Parallel()
	client := twoHosts(t, "shared", "shared")
	seen, err := runScripted(t, client, true, map[string][]scriptStep{"a": {notFound430()}})
	if !IsNotFoundEverywhere(err) || !slices.Equal(seen, []string{"a"}) {
		t.Fatalf("err = %v, seen = %v", err, seen)
	}
}

func TestFailoverSameHostSecondAccountIsNotProof(t *testing.T) {
	t.Parallel()
	port := livePort(t)
	client := failoverClient(t, 0,
		failoverProvider("127.0.0.1", port, "a", 1, "", false),
		failoverProvider("127.0.0.1", port, "b", 2, "", false),
	)
	seen, err := runScripted(t, client, true, map[string][]scriptStep{"a": {notFound430()}})
	if !IsArticleNotFoundError(err) || IsNotFoundEverywhere(err) || !slices.Equal(seen, []string{"a"}) {
		t.Fatalf("err = %v, seen = %v", err, seen)
	}
}

func TestFailoverBackupCoverage(t *testing.T) {
	t.Parallel()
	port := livePort(t)
	unlabelled := failoverClient(t, 0,
		failoverProvider("127.0.0.1", port, "a", 1, "", false),
		failoverProvider("127.0.0.1", port, "b", 2, "", true),
	)
	_, err := runScripted(t, unlabelled, true, map[string][]scriptStep{"a": {notFound430()}})
	if IsNotFoundEverywhere(err) {
		t.Fatalf("unasked backup counted as covered: %v", err)
	}
	labelled := failoverClient(t, 0,
		failoverProvider("127.0.0.1", port, "a", 1, "shared", false),
		failoverProvider("127.0.0.1", port, "b", 2, "shared", true),
	)
	_, err = runScripted(t, labelled, true, map[string][]scriptStep{"a": {notFound430()}})
	if !IsNotFoundEverywhere(err) {
		t.Fatalf("labelled backup not covered: %v", err)
	}
}

func TestFailoverRecoveredAcquireFailureIsNotProof(t *testing.T) {
	t.Parallel()
	client := failoverClient(t, 0,
		failoverProvider("localhost", deadPort(t), "a", 1, "shared", false),
		failoverProvider("127.0.0.1", livePort(t), "b", 2, "shared", false),
	)
	seen, err := runScripted(t, client, true, map[string][]scriptStep{"b": {notFound430()}})
	if !IsArticleNotFoundError(err) || IsNotFoundEverywhere(err) || !slices.Equal(seen, []string{"b"}) {
		t.Fatalf("err = %v, seen = %v", err, seen)
	}
}

func TestFailoverTerminalAcquireFailureIsNotProof(t *testing.T) {
	t.Parallel()
	client := failoverClient(t, 0,
		failoverProvider("127.0.0.1", livePort(t), "a", 1, "", false),
		failoverProvider("localhost", deadPort(t), "b", 2, "", true),
	)
	_, err := runScripted(t, client, true, map[string][]scriptStep{"a": {notFound430()}})
	if err == nil || IsNotFoundEverywhere(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestFailoverTransientThenNotFoundIsNotProof(t *testing.T) {
	t.Parallel()
	transient := raised(NewTimeoutError(errors.New("timeout")))
	_, err := runScripted(t, failoverClientRetries(t), true, map[string][]scriptStep{
		"a": {transient, notFound430()},
		"b": {notFound430()},
	})
	if !IsArticleNotFoundError(err) || IsNotFoundEverywhere(err) {
		t.Fatalf("err = %v", err)
	}
}

func failoverClientRetries(t *testing.T) *Client {
	t.Helper()
	port := livePort(t)
	return failoverClient(t, 1,
		failoverProvider("127.0.0.1", port, "a", 1, "", false),
		failoverProvider("localhost", port, "b", 2, "", false),
	)
}

func TestFailoverNotFoundThenTransientIsNotProof(t *testing.T) {
	t.Parallel()
	transient := raised(NewTimeoutError(errors.New("timeout")))
	client := failoverClientRetries(t)
	_, err := runScripted(t, client, true, map[string][]scriptStep{
		"a": {notFound430()},
		"b": {transient},
	})
	if err == nil || IsNotFoundEverywhere(err) || IsArticleNotFoundError(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestFailoverSingleProviderTransientThenNotFoundIsNotProof(t *testing.T) {
	t.Parallel()
	client := failoverClient(t, 1, failoverProvider("127.0.0.1", livePort(t), "a", 1, "", false))
	seen, err := runScripted(t, client, true, map[string][]scriptStep{
		"a": {raised(NewTimeoutError(errors.New("timeout"))), notFound430()},
	})
	if !slices.Equal(seen, []string{"a", "a"}) || !IsArticleNotFoundError(err) || IsNotFoundEverywhere(err) {
		t.Fatalf("err = %v, seen = %v", err, seen)
	}
}

func TestFailover423IsNotProof(t *testing.T) {
	t.Parallel()
	notInGroup := scriptStep{reply: "423"}
	_, err := runScripted(t, twoHosts(t, "", ""), true, map[string][]scriptStep{"a": {notInGroup}, "b": {notInGroup}})
	if err == nil || IsNotFoundEverywhere(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestFailoverNoDataBodyIsNotProof(t *testing.T) {
	t.Parallel()
	noData := scriptStep{reply: scriptBodyCode}
	_, err := runScripted(t, twoHosts(t, "", ""), true, map[string][]scriptStep{"a": {noData}, "b": {notFound430()}})
	if err == nil || IsNotFoundEverywhere(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestStatProves(t *testing.T) {
	t.Parallel()
	port := livePort(t)
	client := failoverClient(t, 0,
		failoverProvider("127.0.0.1", port, "a", 1, "", false),
		failoverProvider("localhost", port, "b", 2, "", false),
	)
	_, _, answer, err := client.Stat(t.Context(), WorkloadDownload, "missing@example")
	if !IsNotFoundEverywhere(err) || answer.Kind() != AnswerMissing {
		t.Fatalf("answer = %v, err = %v", answer, err)
	}
}

func TestFailoverJoined430IsNotProof(t *testing.T) {
	t.Parallel()
	joined := wrapped430(func(err error) error { return errors.Join(err, context.DeadlineExceeded) })
	_, err := runScripted(t, twoHosts(t, "", ""), true, map[string][]scriptStep{"a": {joined}, "b": {notFound430()}})
	if err == nil || IsNotFoundEverywhere(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestIsNotFoundEverywhereRefusesAJoin(t *testing.T) {
	t.Parallel()
	proof := &NotFoundEverywhereError{Err: bare430()}
	if !IsNotFoundEverywhere(fmt.Errorf("stat: %w", proof)) {
		t.Fatal("a wrapped proof is not read")
	}
	if IsNotFoundEverywhere(errors.Join(proof, context.DeadlineExceeded)) {
		t.Fatal("a joined proof is read as proof")
	}
}

func TestFailoverTimeoutAround430IsNotProof(t *testing.T) {
	t.Parallel()
	timeout := wrapped430(func(err error) error { return NewTimeoutError(err) })
	_, err := runScripted(t, twoHosts(t, "", ""), true, map[string][]scriptStep{"a": {timeout}, "b": {notFound430()}})
	if err == nil || IsNotFoundEverywhere(err) {
		t.Fatalf("err = %v", err)
	}
}

type embeddedProofJoinError struct {
	*NotFoundEverywhereError

	other error
}

func (e embeddedProofJoinError) Unwrap() []error { return []error{e.NotFoundEverywhereError, e.other} }

func TestIsNotFoundEverywhereRefusesAnEmbeddedProof(t *testing.T) {
	t.Parallel()
	proof := &NotFoundEverywhereError{Err: bare430()}
	if IsNotFoundEverywhere(embeddedProofJoinError{NotFoundEverywhereError: proof, other: context.DeadlineExceeded}) {
		t.Fatal("an embedded proof beside a deadline is read as proof")
	}
}

type wireError = Error

type aliasEmbedded430Error struct {
	*wireError

	other error
}

func (e aliasEmbedded430Error) Unwrap() []error { return []error{e.wireError, e.other} }

func TestFailoverAliasEmbedded430BesideADeadlineIsNotProof(t *testing.T) {
	t.Parallel()
	mixed := wrapped430(func(err error) error {
		inner, ok := errors.AsType[*Error](err)
		if !ok {
			t.Error("wire 430 is not an *Error")
		}
		return aliasEmbedded430Error{wireError: inner, other: context.DeadlineExceeded}
	})
	_, err := runScripted(t, twoHosts(t, "", ""), true, map[string][]scriptStep{"a": {mixed}, "b": {notFound430()}})
	if err == nil || IsNotFoundEverywhere(err) {
		t.Fatalf("err = %v", err)
	}
}

type aliasEmbeddedTimeoutError struct {
	*wireError
}

type embeddedProofError struct {
	*NotFoundEverywhereError
}

func TestEmbeddedMarkerRefusesInsteadOfSkipping(t *testing.T) {
	t.Parallel()
	timeout := NewTimeoutError(bare430())
	if isNoSuchArticle(aliasEmbeddedTimeoutError{wireError: timeout}) {
		t.Fatal("an embedded timeout around a 430 is read as a 430")
	}
	proof := &NotFoundEverywhereError{Err: &NotFoundEverywhereError{Err: bare430()}}
	if IsNotFoundEverywhere(embeddedProofError{NotFoundEverywhereError: proof}) {
		t.Fatal("an embedded proof is read through its promoted Unwrap")
	}
}

type timeoutWrapError struct{ err error }

func (e timeoutWrapError) Error() string { return "timed out: " + e.err.Error() }

func (e timeoutWrapError) Unwrap() error { return e.err }

func (timeoutWrapError) Timeout() bool { return true }

type quietWrapError struct{ timeoutWrapError }

func (quietWrapError) Timeout() bool { return false }

func TestTimeoutAroundAMarkerRefuses(t *testing.T) {
	t.Parallel()
	proof := &NotFoundEverywhereError{Err: bare430()}
	for _, row := range []struct {
		name  string
		err   error
		check func(error) bool
		want  bool
	}{
		{"430 alone", bare430(), isNoSuchArticle, true},
		{"timeout around a 430", timeoutWrapError{bare430()}, isNoSuchArticle, false},
		{"timeout around a wrapped 430", timeoutWrapError{fmt.Errorf("read: %w", bare430())}, isNoSuchArticle, false},
		{"non-timeout wrapper around a 430", quietWrapError{timeoutWrapError{bare430()}}, isNoSuchArticle, true},
		{"proof alone", proof, IsNotFoundEverywhere, true},
		{"timeout around a proof", timeoutWrapError{proof}, IsNotFoundEverywhere, false},
		{"wrapped timeout around a proof", fmt.Errorf("stat: %w", timeoutWrapError{proof}), IsNotFoundEverywhere, false},
		{"non-timeout wrapper around a proof", quietWrapError{timeoutWrapError{proof}}, IsNotFoundEverywhere, true},
		{"nntp timeout around a proof", NewTimeoutError(proof), IsNotFoundEverywhere, false}} {
		if got := row.check(row.err); got != row.want {
			t.Errorf("%s: got %v, want %v", row.name, got, row.want)
		}
	}
}

func TestFailoverTimeoutWrapped430IsNotProof(t *testing.T) {
	t.Parallel()
	_, err := runScripted(t, twoHosts(t, "", ""), true, map[string][]scriptStep{
		"a": {wrapped430(func(err error) error { return timeoutWrapError{err} })},
		"b": {notFound430()},
	})
	if IsNotFoundEverywhere(err) {
		t.Fatalf("a timeout around a 430 counted as a miss: %v", err)
	}
}

func TestFailoverCancelledDuringTheAttemptIsFailedWithCtxError(t *testing.T) {
	t.Parallel()
	transient := NewTimeoutError(errors.New("timeout"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	answer, err := twoHosts(t, "", "").ExecuteArticle(ctx, WorkloadDownload, func(*Connection) error {
		cancel()
		return transient
	})
	if answer.Kind() != AnswerFailed || !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecuteArticle = (%v, %v), want (AnswerFailed, context.Canceled)", answer, err)
	}
}

func TestFailoverCancelAroundTheBackoffSleep(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name         string
		timerFirst   bool
		wantAttempts int32
	}{
		{"cancel while the sleep waits kills Found() on the sleepCtx return", false, 1},
		{"sleep ends, next attempt cancelled kills Found() on the post-attempt ctx check", true, 2},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			port := livePort(t)
			synctest.Test(t, func(t *testing.T) {
				answer, attempts, err := cancelAroundBackoff(t, port, row.timerFirst)
				if answer.Kind() != AnswerFailed || !errors.Is(err, context.Canceled) {
					t.Fatalf("ExecuteArticle = (%v, %v), want (AnswerFailed, context.Canceled)", answer, err)
				}
				if attempts != row.wantAttempts {
					t.Fatalf("attempts = %d, want %d", attempts, row.wantAttempts)
				}
			})
		})
	}
}

func cancelAroundBackoff(t *testing.T, port int, timerFirst bool) (Answer, int32, error) {
	t.Helper()
	transient := NewTimeoutError(errors.New("timeout"))
	client := failoverClient(t, 1, failoverProvider("127.0.0.1", port, "a", 1, "", false))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var attempts atomic.Int32
	fn := func(*Connection) error {
		if attempts.Add(1) == 2 {
			cancel()
		}
		return transient
	}
	type outcome struct {
		answer Answer
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		answer, err := client.ExecuteArticle(ctx, WorkloadDownload, fn)
		done <- outcome{answer, err}
	}()
	synctest.Wait()
	if attempts.Load() != 1 {
		t.Fatalf("attempts before the sleep = %d, want 1", attempts.Load())
	}
	if timerFirst {
		<-time.After(retryDelay(1))
	} else {
		cancel()
	}
	got := <-done
	return got.answer, attempts.Load(), got.err
}
