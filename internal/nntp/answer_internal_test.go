package nntp

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
)

const answerID = "<article@answer>"

func answerServer(t *testing.T, reply func(command, messageID string) string, articles map[string][]byte) int {
	t.Helper()
	server, err := nntpd.New(nntpd.Config{Reply: reply})
	if err != nil {
		t.Fatal(err)
	}
	for id, body := range articles {
		server.AddArticle(id, body)
	}
	t.Cleanup(server.Close)
	_, port := server.Addr()
	return port
}

func answering(line string) func(command, messageID string) string {
	return func(command, _ string) string {
		if command == "STAT" || command == "BODY" {
			return line
		}
		return ""
	}
}

func rejectingAuth(command, _ string) string {
	if command == "AUTHINFO" {
		return "430 rejected"
	}
	return ""
}

func badCRCBody(t *testing.T) []byte {
	t.Helper()
	body := nntpd.Encode(nntpd.Pattern(0, 64), "f.bin", 1, 64, 0)
	marker := []byte("pcrc32=")
	at := bytes.Index(body, marker)
	if at < 0 {
		t.Fatal("encoded body has no pcrc32")
	}
	at += len(marker)
	if bytes.Equal(body[at:at+8], []byte("ffffffff")) {
		t.Fatal("fixture crc already matches the corruption")
	}
	copy(body[at:], "ffffffff")
	return body
}

func goodBody() []byte {
	return nntpd.Encode(nntpd.Pattern(0, 64), "f.bin", 1, 64, 0)
}

func statArticle(conn *Connection) error {
	_, _, err := conn.Stat(answerID)
	return err
}

func rawBodyArticle(conn *Connection) error {
	_, err := conn.GetBody(answerID)
	return err
}

func batchStatArticle(conn *Connection) error {
	results, err := conn.StatBatch([]string{answerID})
	if err != nil {
		return err
	}
	return results[0].Error
}

func bodyArticle(conn *Connection) error {
	_, err := conn.requestBody(answerID)
	return err
}

type answerRow struct {
	name      string
	build     func(t *testing.T) (*Client, func(*Connection) error)
	cancelled bool
	workload  Workload
	want      Answer
	missing   int
}

func twoServers(t *testing.T, a, b func(string, string) string, articlesA, articlesB map[string][]byte) *Client {
	t.Helper()
	return failoverClient(t, 0,
		failoverProvider("127.0.0.1", answerServer(t, a, articlesA), "a", 1, "", false),
		failoverProvider("localhost", answerServer(t, b, articlesB), "b", 2, "", false),
	)
}

func answerRows() []answerRow {
	allReply := func(line string, fn func(*Connection) error) func(*testing.T) (*Client, func(*Connection) error) {
		return func(t *testing.T) (*Client, func(*Connection) error) {
			return twoServers(t, answering(line), answering(line), nil, nil), fn
		}
	}
	return []answerRow{
		{name: "430 on every provider", build: allReply("430 gone", statArticle), want: AnswerMissing, missing: 2},
		{
			name:  "raw BODY 430 on every provider",
			build: allReply("430 gone", rawBodyArticle),
			want:  AnswerMissing, missing: 2,
		},
		{
			name:  "one-item StatBatch 430 on every provider",
			build: allReply("430 gone", batchStatArticle),
			want:  AnswerMissing, missing: 2,
		},
		{
			name: "dead backup, live primary 430",
			build: func(t *testing.T) (*Client, func(*Connection) error) {
				return failoverClient(t, 0,
					failoverProvider("127.0.0.1", answerServer(t, answering("430 gone"), nil), "a", 1, "", false),
					failoverProvider("localhost", deadPort(t), "b", 2, "", true),
				), statArticle
			},
			want: AnswerFailed, missing: 1,
		},
		{
			name: "live primary 430, backup AUTHINFO 430",
			build: func(t *testing.T) (*Client, func(*Connection) error) {
				return failoverClient(t, 0,
					failoverProvider("127.0.0.1", answerServer(t, answering("430 gone"), nil), "a", 1, "", false),
					failoverProvider("localhost", answerServer(t, rejectingAuth, nil), "b", 2, "", true),
				), statArticle
			},
			want: AnswerNotFound, missing: 1,
		},
		{
			name: "dead primary then live shared-backbone 430",
			build: func(t *testing.T) (*Client, func(*Connection) error) {
				return failoverClient(t, 0,
					failoverProvider("localhost", deadPort(t), "a", 1, "shared", false),
					failoverProvider("127.0.0.1", answerServer(t, answering("430 gone"), nil), "b", 2, "shared", false),
				), statArticle
			},
			want: AnswerNotFound, missing: 1,
		},
		{
			name: "dead primary then live 430 on another backbone",
			build: func(t *testing.T) (*Client, func(*Connection) error) {
				return failoverClient(t, 0,
					failoverProvider("localhost", deadPort(t), "a", 1, "", false),
					failoverProvider("127.0.0.1", answerServer(t, answering("430 gone"), nil), "b", 2, "", false),
				), statArticle
			},
			want: AnswerFailed, missing: 1,
		},
		{name: "423 on every provider", build: allReply("423 no number", statArticle), want: AnswerNotFound},
		{
			name: "222 with a bad yEnc CRC",
			build: func(t *testing.T) (*Client, func(*Connection) error) {
				bad := map[string][]byte{answerID: badCRCBody(t)}
				return twoServers(t, nil, nil, bad, bad), bodyArticle
			},
			want: AnswerCorrupt,
		},
		{
			name: "222 with no yEnc data",
			build: func(t *testing.T) (*Client, func(*Connection) error) {
				empty := map[string][]byte{answerID: []byte("no yenc data here\r\n")}
				return twoServers(t, nil, nil, empty, empty), bodyArticle
			},
			want: AnswerNotFound,
		},
		{
			name: "provider 1 corrupt, provider 2 430",
			build: func(t *testing.T) (*Client, func(*Connection) error) {
				bad := map[string][]byte{answerID: badCRCBody(t)}
				return twoServers(t, nil, answering("430 gone"), bad, nil), bodyArticle
			},
			want: AnswerNotFound, missing: 1,
		},
		{
			name: "provider 1 430, provider 2 corrupt",
			build: func(t *testing.T) (*Client, func(*Connection) error) {
				bad := map[string][]byte{answerID: badCRCBody(t)}
				return twoServers(t, answering("430 gone"), nil, nil, bad), bodyArticle
			},
			want: AnswerCorrupt, missing: 1,
		},
		{
			name: "timeoutWrapError around the wire 430",
			build: allReply("430 gone", func(conn *Connection) error {
				return timeoutWrapError{statArticle(conn)}
			}),
			want: AnswerNotFound,
		},
		{
			name: "NewTimeoutError around the wire 430",
			build: allReply("430 gone", func(conn *Connection) error {
				return NewTimeoutError(statArticle(conn))
			}),
			want: AnswerFailed,
		},
		{
			name: "wire 430 then panic with the error",
			build: allReply("430 gone", func(conn *Connection) error {
				panic(statArticle(conn))
			}),
			want: AnswerNotFound,
		},
		{
			name:  "panic with a non-error value",
			build: allReply("430 gone", func(*Connection) error { panic("x") }),
			want:  AnswerFailed,
		},
		{
			name: "two STATs, last 430",
			build: allReply("430 gone", func(conn *Connection) error {
				_ = statArticle(conn)
				return statArticle(conn)
			}),
			want: AnswerNotFound,
		},
		{
			name: "StatBatch then Stat on one connection",
			build: allReply("430 gone", func(conn *Connection) error {
				if _, err := conn.StatBatch([]string{answerID}); err != nil {
					return err
				}
				return statArticle(conn)
			}),
			want: AnswerNotFound,
		},
		{
			name:  "fatal error",
			build: allReply("430 gone", func(*Connection) error { return errors.New("fatal") }),
			want:  AnswerFailed,
		},
		{name: "ctx cancelled", build: allReply("430 gone", statArticle), cancelled: true, want: AnswerFailed},
		{
			name:     "invalid workload",
			build:    allReply("430 gone", statArticle),
			workload: workloadCount,
			want:     AnswerFailed,
		},
		{
			name: "served",
			build: func(t *testing.T) (*Client, func(*Connection) error) {
				found := map[string][]byte{answerID: goodBody()}
				return twoServers(t, nil, nil, found, found), statArticle
			},
			want: AnswerFound,
		},
	}
}

func TestAnswerTable(t *testing.T) {
	t.Parallel()
	for _, row := range answerRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			client, fn := row.build(t)
			ctx := t.Context()
			if row.cancelled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, st, err := client.executeState(ctx, row.workload, fn)
			if got != row.want || len(st.missingIDs) != row.missing {
				t.Fatalf("answer = %v (want %v), counted = %d (want %d), err = %v",
					got, row.want, len(st.missingIDs), row.missing, err)
			}
			if (got == AnswerFound) != (err == nil) {
				t.Fatalf("answer %v with err %v", got, err)
			}
		})
	}
}

func TestExecuteArticleWrapsExactlyWhenMissing(t *testing.T) {
	t.Parallel()
	for _, row := range answerRows() {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			client, fn := row.build(t)
			ctx := t.Context()
			if row.cancelled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := client.ExecuteArticle(ctx, row.workload, fn)
			if got != row.want || IsNotFoundEverywhere(err) != (got == AnswerMissing) {
				t.Fatalf("answer = %v (want %v), proof = %v, err = %v", got, row.want, IsNotFoundEverywhere(err), err)
			}
		})
	}
}

func TestTrackNeedsCompletedCommand(t *testing.T) {
	t.Parallel()
	provider := failoverProvider("127.0.0.1", 1, "a", 1, "", false)
	notFound := &Error{Type: ErrorTypeArticleNotFound, Code: 430}

	var open failoverState
	open.track(provider, attemptExcludeMissing, commandResult{code: 430, count: 1, completed: false}, notFound)
	if len(open.missingIDs) != 0 || !open.unproven {
		t.Fatalf("uncompleted command: counted %d, unproven %v", len(open.missingIDs), open.unproven)
	}

	var done failoverState
	done.track(provider, attemptExcludeMissing, commandResult{code: 430, count: 1, completed: true}, notFound)
	if len(done.missingIDs) != 1 || done.unproven {
		t.Fatalf("completed command: counted %d, unproven %v", len(done.missingIDs), done.unproven)
	}
}

func TestTrackCountsWireCodeNotErrorShape(t *testing.T) {
	t.Parallel()
	provider := failoverProvider("127.0.0.1", 1, "a", 1, "", false)

	var hidden failoverState
	hidden.track(provider, attemptExcludeMissing, commandResult{code: 430, count: 1, completed: true},
		&Error{Type: ErrorTypeArticleNotFound})
	if len(hidden.missingIDs) != 1 {
		t.Fatalf("wire 430 under a code-0 error was not counted: %d", len(hidden.missingIDs))
	}

	var claimed failoverState
	claimed.track(provider, attemptExcludeMissing, commandResult{code: 222, count: 1, completed: true},
		&Error{Type: ErrorTypeArticleNotFound, Code: 430})
	if len(claimed.missingIDs) != 0 || !claimed.unproven {
		t.Fatalf("error claiming 430 after a 222 was counted: %d", len(claimed.missingIDs))
	}
}

func TestStaleCommandResultIsNotEvidence(t *testing.T) {
	t.Parallel()
	client := failoverClient(t, 0,
		failoverProvider("127.0.0.1", answerServer(t, answering("430 gone"), nil), "a", 1, "", false),
	)
	var first, second *Connection
	answer, _, _ := client.executeState(t.Context(), WorkloadDownload, func(conn *Connection) error {
		first = conn
		return statArticle(conn)
	})
	if answer != AnswerMissing {
		t.Fatalf("first call answer = %v", answer)
	}
	answer, st, _ := client.executeState(t.Context(), WorkloadDownload, func(conn *Connection) error {
		second = conn
		return &Error{Type: ErrorTypeArticleNotFound, Code: 430}
	})
	if first == nil || first != second {
		t.Fatal("the second call did not borrow the pooled connection")
	}
	if answer != AnswerNotFound || len(st.missingIDs) != 0 {
		t.Fatalf("answer = %v, counted = %d", answer, len(st.missingIDs))
	}
}
