package usenet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/logger"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/parser"
)

const (
	warnAvailabilityFailed = "Post-parse availability check failed; marking NZB unavailable"
	warnFileUnavailable    = "File is unavailable - one or more segments are missing"
	warnStatNonFatal       = "Non-fatal error during availability check, ignoring"
	warnContentFailed      = "Content verification failed; failing NZB"
	warnContentNonFatal    = "Non-fatal error during content verification, ignoring"
)

type warnSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *warnSink) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *warnSink) count(message string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Count(w.buf.String(), message)
}

func (w *warnSink) total() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Count(w.buf.String(), "| WARN")
}

type probeLog struct {
	mu  sync.Mutex
	ids map[string][]string
}

func (p *probeLog) record(command, messageID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids[command] = append(p.ids[command], messageID)
}

func (p *probeLog) sorted(command string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := slices.Clone(p.ids[command])
	slices.Sort(out)
	return slices.Compact(out)
}

type pinRig struct {
	u      *Usenet
	srv    *nntpd.Server
	root   *task.Group
	warns  *warnSink
	probes *probeLog
}

func newPinRig(t *testing.T, reply func(command, messageID string) string) *pinRig {
	t.Helper()
	rig := &pinRig{warns: &warnSink{}, probes: &probeLog{ids: map[string][]string{}}}
	srv, err := nntpd.New(nntpd.Config{Reply: func(command, messageID string) string {
		rig.probes.record(command, messageID)
		if reply == nil {
			return ""
		}
		return reply(command, messageID)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	_, port := srv.Addr()
	rig.srv = srv
	rig.u, rig.root = newLifecycleUsenetLogged(t, port, 2, logger.NewFactory("warn", nil, rig.warns))
	return rig
}

func (r *pinRig) serve(t *testing.T, payload []byte) {
	t.Helper()
	r.srv.AddArticle("<head@gate>", nntpd.Encode(payload, "Movie.mkv", 1, int64(len(payload)), 0))
}

func (r *pinRig) assertWarns(t *testing.T, want map[string]int) {
	t.Helper()
	total := 0
	for message, n := range want {
		if got := r.warns.count(message); got != n {
			t.Errorf("warning %q logged %d times, want %d", message, got, n)
		}
		total += n
	}
	if got := r.warns.total(); got != total {
		t.Errorf("%d warnings logged, want %d", got, total)
	}
}

func status400(command, _ string) string {
	if command == "STAT" || command == "BODY" {
		return "400 service unavailable"
	}
	return ""
}

func statGoneReply(command, _ string) string {
	if command == "STAT" {
		return "430 no such article"
	}
	return ""
}

func statDownReply(command, _ string) string {
	if command == "STAT" {
		return "400 service unavailable"
	}
	return ""
}

func bodyDownReply(command, _ string) string {
	if command == "BODY" {
		return "400 service unavailable"
	}
	return ""
}

func bodyGoneReply(command, _ string) string {
	if command == "BODY" {
		return "430 no such article"
	}
	return ""
}

var (
	junkPayload = []byte(strings.Repeat("not a media container ", 40))
	mkvPayload  = append([]byte{0x1A, 0x45, 0xDF, 0xA3}, make([]byte, 600)...)
	mkvHead16   = append([]byte{0x1A, 0x45, 0xDF, 0xA3}, make([]byte, 12)...)
	junkHead16  = []byte("not a container!")
)

func sentinelText(err error) string { return err.Error() }

func segmentedFile(n int) storage.NZBFile {
	file := storage.NZBFile{Name: "Movie.mkv", FileType: storage.NZBFileTypeMedia, Size: int64(n) * 5}
	for i := range n {
		file.Segments = append(file.Segments, storage.NZBSegment{
			Number: i + 1, MessageID: fmt.Sprintf("<s%d@gate>", i+1), Bytes: 5,
			StartOffset: int64(i) * 5, EndOffset: int64(i)*5 + 4,
		})
	}
	return file
}

func unknownSizeFile(size int) storage.NZBFile {
	file := gateFile(size)
	file.Size = 0
	return file
}

type tuplePin struct {
	missing bool
	err     string
	is      error
	isNot   []error
}

func (w tuplePin) check(t *testing.T, missing bool, err error) {
	t.Helper()
	if missing != w.missing {
		t.Errorf("missing = %v, want %v (error %v)", missing, w.missing, err)
	}
	if w.err == "" && w.is == nil && err != nil {
		t.Errorf("error = %v, want nil", err)
		return
	}
	if w.err != "" || w.is != nil {
		if err == nil {
			t.Errorf("error = nil, want %q / %v", w.err, w.is)
			return
		}
	}
	if w.err != "" && !strings.Contains(err.Error(), w.err) {
		t.Errorf("error = %v, want it to contain %q", err, w.err)
	}
	if w.is != nil && !errors.Is(err, w.is) {
		t.Errorf("error = %v, want it to wrap %v", err, w.is)
	}
	for _, not := range w.isNot {
		if errors.Is(err, not) {
			t.Errorf("error = %v, must not wrap %v", err, not)
		}
	}
}

var neitherSentinel = []error{customerror.ErrUsenetSegmentMissing, customerror.ErrUsenetCorruptContent}

func TestPinCheckNZBAvailability(t *testing.T) {
	t.Parallel()
	missingErr := `file "Movie.mkv" unavailable: ` + sentinelText(customerror.ErrUsenetSegmentMissing)
	for _, tc := range []struct {
		name    string
		reply   func(command, messageID string) string
		serve   bool
		ended   bool
		stopped bool
		want    tuplePin
		warns   map[string]int
		probed  bool
	}{
		{name: "every article available", serve: true, want: tuplePin{}, probed: true},
		{name: "context ended", serve: true, ended: true, want: tuplePin{}},
		{
			name: "article gone", reply: statGoneReply, serve: true,
			want: tuplePin{missing: true, err: missingErr, is: customerror.ErrUsenetSegmentMissing},
			warns: map[string]int{
				warnAvailabilityFailed: 1,
				warnFileUnavailable:    1,
			},
			probed: true,
		},
		{name: "connection errors only", reply: statDownReply, serve: true, want: tuplePin{}, probed: true},
		{
			name: "client stopped", serve: true, stopped: true, want: tuplePin{},
			warns: map[string]int{warnStatNonFatal: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newPinRig(t, tc.reply)
			if tc.serve {
				rig.serve(t, junkPayload)
			}
			nzb := gateNZB(t, rig.u, gateFile(len(junkPayload)))
			if tc.stopped {
				stopLifecycleGroup(t, rig.root)
			}
			ctx := t.Context()
			if tc.ended {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			missing, err := rig.u.checkNZBAvailability(ctx, nzb)
			tc.want.check(t, missing, err)
			if tc.want.err != "" && err != nil && err.Error() != tc.want.err {
				t.Errorf("error = %q, want exactly %q", err, tc.want.err)
			}
			rig.assertWarns(t, tc.warns)
			if got := len(rig.probes.sorted("STAT")) > 0; got != tc.probed {
				t.Errorf("probed by STAT = %v, want %v", got, tc.probed)
			}
		})
	}
}

func TestPinStatAvailabilityMixedAndLogs(t *testing.T) {
	t.Parallel()
	mixed := func(command, messageID string) string {
		if command == "STAT" && messageID == "<s1@gate>" {
			return "400 service unavailable"
		}
		return ""
	}
	for _, tc := range []struct {
		name  string
		reply func(command, messageID string) string
		ids   []string
		serve bool
		want  tuplePin
		warns map[string]int
	}{
		{
			name: "one definitive miss and one connection error", reply: mixed,
			ids:   []string{"<s1@gate>", "<s2@gate>"},
			want:  tuplePin{missing: true, is: customerror.ErrUsenetSegmentMissing},
			warns: map[string]int{warnFileUnavailable: 1},
		},
		{
			name: "connection errors only are silent", reply: statDownReply,
			ids: []string{"<head@gate>"}, serve: true, want: tuplePin{},
		},
		{
			name: "every article available is silent",
			ids:  []string{"<head@gate>"}, serve: true, want: tuplePin{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newPinRig(t, tc.reply)
			if tc.serve {
				rig.serve(t, junkPayload)
			}
			missing, err := rig.u.statAvailability(t.Context(), "Movie.mkv", tc.ids)
			tc.want.check(t, missing, err)
			if tc.want.is != nil && err != tc.want.is {
				t.Errorf("error = %v, want the bare sentinel", err)
			}
			rig.assertWarns(t, tc.warns)
		})
	}
}

func TestPinCheckHeadSignature(t *testing.T) {
	t.Parallel()
	err := checkHeadSignature("Movie.mkv", mkvPayload)
	if err != nil {
		t.Fatalf("checkHeadSignature(valid) = %v, want nil", err)
	}
	err = checkHeadSignature("Movie.mkv", junkPayload)
	want := `head of "Movie.mkv" matches no media container signature: ` +
		sentinelText(customerror.ErrUsenetCorruptContent)
	if err == nil || err.Error() != want || !errors.Is(err, customerror.ErrUsenetCorruptContent) {
		t.Fatalf("checkHeadSignature(junk) = %v, want %q", err, want)
	}
}

func TestPinVerifyFileHeadRoutes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		file    storage.NZBFile
		payload []byte
		reply   func(command, messageID string) string
		bare    bool
		want    tuplePin
		bodies  bool
	}{
		{name: "analyzer nil, valid head", file: gateFile(len(mkvPayload)), payload: mkvPayload, bare: true, bodies: true},
		{name: "analyzer success, valid head", file: gateFile(len(mkvPayload)), payload: mkvPayload, bodies: true},
		{
			name: "analyzer success, no signature", file: gateFile(len(junkPayload)), payload: junkPayload, bodies: true,
			want: tuplePin{is: customerror.ErrUsenetCorruptContent},
		},
		{
			name: "cursor partial EOF, unknown size, valid 16-byte head", file: unknownSizeFile(16),
			payload: mkvHead16, bare: true, bodies: true,
		},
		{
			name: "cursor partial EOF, unknown size, 16 bytes with no signature", file: unknownSizeFile(16),
			payload: junkHead16, bare: true, bodies: true,
			want: tuplePin{is: customerror.ErrUsenetCorruptContent},
		},
		{
			name: "analyzer partial coverage, unknown size", file: unknownSizeFile(16), payload: mkvHead16, bodies: true,
			want: tuplePin{err: "partially covered", isNot: neitherSentinel},
		},
		{
			name: "cursor short head, unknown size", file: unknownSizeFile(8), payload: []byte("12345678"),
			bare: true, bodies: true,
			want: tuplePin{err: "EOF", isNot: neitherSentinel},
		},
		{
			name: "cursor, connection error", file: gateFile(len(mkvPayload)), payload: mkvPayload, bare: true,
			reply: bodyDownReply, bodies: true, want: tuplePin{err: "400", isNot: neitherSentinel},
		},
		{
			name: "analyzer, connection error", file: gateFile(len(mkvPayload)), payload: mkvPayload,
			reply: bodyDownReply, bodies: true, want: tuplePin{err: "400", isNot: neitherSentinel},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newPinRig(t, tc.reply)
			rig.serve(t, tc.payload)
			if tc.bare {
				rig.u.analyzer = nil
			}
			missing, err := rig.u.verifyFileHead(t.Context(), &tc.file)
			tc.want.check(t, missing, err)
			if tc.bodies && len(rig.probes.sorted("BODY")) == 0 {
				t.Error("the body was never fetched")
			}
		})
	}
}

func TestPinVerifyNZBContent(t *testing.T) {
	t.Parallel()
	headMissing := `head article of "Movie.mkv" missing: ` + sentinelText(customerror.ErrUsenetSegmentMissing)
	noSignature := `head of "Movie.mkv" matches no media container signature: ` +
		sentinelText(customerror.ErrUsenetCorruptContent)
	for _, tc := range []struct {
		name    string
		reply   func(command, messageID string) string
		payload []byte
		serve   bool
		ended   bool
		bare    bool
		want    tuplePin
		exact   string
		warns   map[string]int
	}{
		{name: "context ended", payload: junkPayload, serve: true, ended: true},
		{name: "valid head", payload: mkvPayload, serve: true},
		{
			name: "head missing", reply: bodyGoneReply, payload: junkPayload, serve: true,
			want:  tuplePin{missing: true, is: customerror.ErrUsenetSegmentMissing},
			exact: headMissing, warns: map[string]int{warnContentFailed: 1},
		},
		{
			name: "head missing via the cursor", reply: bodyGoneReply, payload: junkPayload, serve: true, bare: true,
			want:  tuplePin{missing: true, is: customerror.ErrUsenetSegmentMissing},
			exact: headMissing, warns: map[string]int{warnContentFailed: 1},
		},
		{
			name: "no signature", payload: junkPayload, serve: true,
			want:  tuplePin{is: customerror.ErrUsenetCorruptContent},
			exact: noSignature, warns: map[string]int{warnContentFailed: 1},
		},
		{
			name: "connection error is not a verdict", reply: bodyDownReply, payload: junkPayload, serve: true,
			warns: map[string]int{warnContentNonFatal: 1},
		},
		{
			name: "connection error via the cursor is not a verdict", reply: bodyDownReply, payload: junkPayload,
			serve: true, bare: true, warns: map[string]int{warnContentNonFatal: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newPinRig(t, tc.reply)
			if tc.serve {
				rig.serve(t, tc.payload)
			}
			if tc.bare {
				rig.u.analyzer = nil
			}
			nzb := gateNZB(t, rig.u, gateFile(len(tc.payload)))
			ctx := t.Context()
			if tc.ended {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			missing, err := rig.u.verifyNZBContent(ctx, nzb)
			tc.want.check(t, missing, err)
			if tc.exact != "" && (err == nil || err.Error() != tc.exact) {
				t.Errorf("error = %v, want exactly %q", err, tc.exact)
			}
			rig.assertWarns(t, tc.warns)
			if tc.ended && len(rig.probes.sorted("BODY")) != 0 {
				t.Error("a body was fetched after the context ended")
			}
		})
	}
}

func TestPinFinishBoundary(t *testing.T) {
	t.Parallel()
	missing := sentinelText(customerror.ErrUsenetSegmentMissing)
	corrupt := sentinelText(customerror.ErrUsenetCorruptContent)
	unavailable := `file "Movie.mkv" unavailable: ` + missing
	headMissing := `head article of "Movie.mkv" missing: ` + missing
	noSignature := `head of "Movie.mkv" matches no media container signature: ` + corrupt
	for _, tc := range []struct {
		name    string
		reply   func(command, messageID string) string
		payload []byte
		bare    bool
		ended   bool
		expired bool
		status  string
		result  ProcessResult
		err     string
		stored  string
	}{
		{
			name: "availability miss", reply: statGoneReply, payload: junkPayload, status: NZBStatusFailed,
			result: ProcessResult{GateMissing: true},
			err:    "availability check failed: " + unavailable, stored: unavailable,
		},
		{
			name: "head miss via the analyzer", reply: bodyGoneReply, payload: junkPayload, status: NZBStatusFailed,
			result: ProcessResult{GateMissing: true},
			err:    "content verification failed: " + headMissing, stored: headMissing,
		},
		{
			name: "head miss via the cursor", reply: bodyGoneReply, payload: junkPayload, bare: true,
			status: NZBStatusFailed, result: ProcessResult{GateMissing: true},
			err: "content verification failed: " + headMissing, stored: headMissing,
		},
		{
			name: "corrupt head", payload: junkPayload, status: NZBStatusFailed,
			err: "content verification failed: " + noSignature, stored: noSignature,
		},
		{name: "availability unchecked", reply: statDownReply, payload: mkvPayload, status: NZBStatusCompleted},
		{name: "content unchecked", reply: bodyDownReply, payload: mkvPayload, status: NZBStatusCompleted},
		{name: "both gates unchecked", reply: status400, payload: mkvPayload, status: NZBStatusCompleted},
		{
			name: "cancelled", payload: mkvPayload, ended: true, status: NZBStatusParsing,
			err: "processing interrupted: context canceled",
		},
		{
			name: "deadline passed", payload: mkvPayload, expired: true, status: NZBStatusFailed,
			err: "processing interrupted: context deadline exceeded", stored: "context deadline exceeded",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newPinRig(t, tc.reply)
			rig.serve(t, tc.payload)
			if tc.bare {
				rig.u.analyzer = nil
			}
			nzb := gateNZB(t, rig.u, gateFile(len(tc.payload)))
			ctx := t.Context()
			switch {
			case tc.ended:
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case tc.expired:
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			got, err := rig.u.finish(ctx, nzb)
			if got != tc.result {
				t.Errorf("ProcessResult = %+v, want %+v", got, tc.result)
			}
			if (tc.err == "") != (err == nil) || (err != nil && err.Error() != tc.err) {
				t.Errorf("error = %v, want exactly %q", err, tc.err)
			}
			if nzb.Status != tc.status {
				t.Errorf("Status = %q, want %q", nzb.Status, tc.status)
			}
			if nzb.FailMessage != tc.stored {
				t.Errorf("FailMessage = %q, want %q", nzb.FailMessage, tc.stored)
			}
		})
	}
}

func TestPinExportedCallers(t *testing.T) {
	t.Parallel()
	missing := sentinelText(customerror.ErrUsenetSegmentMissing)
	for _, tc := range []struct {
		name    string
		reply   func(command, messageID string) string
		payload []byte
		file    storage.NZBFile
		serve   bool
		bare    bool
		want    string
		is      error
		isNot   []error
	}{
		{name: "head too small", payload: junkPayload[:100], file: gateFile(100), serve: true},
		{name: "valid head", payload: mkvPayload, file: gateFile(len(mkvPayload)), serve: true},
		{
			name: "head article gone", reply: bodyGoneReply, payload: junkPayload, file: gateFile(len(junkPayload)),
			serve: true, want: `head article of "Movie.mkv" missing: ` + missing, is: customerror.ErrUsenetSegmentMissing,
		},
		{
			name: "no signature", payload: junkPayload, file: gateFile(len(junkPayload)), serve: true,
			is: customerror.ErrUsenetCorruptContent,
		},
		{
			name: "connection error stays raw", reply: bodyDownReply, payload: mkvPayload,
			file: gateFile(len(mkvPayload)), serve: true, want: "400", isNot: neitherSentinel,
		},
		{
			name: "connection error stays raw via the cursor", reply: bodyDownReply, payload: mkvPayload,
			file: gateFile(len(mkvPayload)), serve: true, bare: true, want: "400", isNot: neitherSentinel,
		},
	} {
		t.Run("VerifyFileHead "+tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newPinRig(t, tc.reply)
			if tc.serve {
				rig.serve(t, tc.payload)
			}
			if tc.bare {
				rig.u.analyzer = nil
			}
			err := rig.u.VerifyFileHead(t.Context(), &tc.file)
			if tc.want == "" && tc.is == nil && len(tc.isNot) == 0 {
				if err != nil {
					t.Fatalf("VerifyFileHead = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("VerifyFileHead = nil, want an error")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("error = %v, want it to wrap %v", err, tc.is)
			}
			for _, not := range tc.isNot {
				if errors.Is(err, not) {
					t.Errorf("error = %v, must not wrap %v", err, not)
				}
			}
		})
	}
}

func storeSegmented(t *testing.T, rig *pinRig, n int, served func(i int) bool) {
	t.Helper()
	file := segmentedFile(n)
	for i := range n {
		if served(i) {
			rig.srv.AddArticle(file.Segments[i].MessageID, nntpd.Encode([]byte("hello"), "Movie.mkv", i+1, int64(n)*5, int64(i)*5))
		}
	}
	gateNZB(t, rig.u, file)
}

func segmentIDs(n int, keep func(i int) bool) []string {
	var out []string
	for i := range n {
		if keep(i) {
			out = append(out, fmt.Sprintf("<s%d@gate>", i+1))
		}
	}
	slices.Sort(out)
	return out
}

func TestPinCheckFileAndWholeFile(t *testing.T) {
	t.Parallel()
	const total = 20
	all := func(int) bool { return true }
	for _, tc := range []struct {
		name      string
		reply     func(command, messageID string) string
		served    func(i int) bool
		wantCheck error
		wantWhole error
		checkIDs  []string
		wholeIDs  []string
	}{
		{
			name: "every article available", served: all,
			checkIDs: segmentIDs(total, func(i int) bool { return i == 0 || i == total-1 }),
			wholeIDs: segmentIDs(total, all),
		},
		{
			name: "interior segment missing outside the sample", served: func(i int) bool { return i != 5 },
			wantWhole: customerror.ErrUsenetSegmentMissing,
			checkIDs:  segmentIDs(total, func(i int) bool { return i == 0 || i == total-1 }),
			wholeIDs:  []string{"<s6@gate>"},
		},
		{
			name: "first segment missing", served: func(i int) bool { return i != 0 },
			wantCheck: customerror.ErrUsenetSegmentMissing, wantWhole: customerror.ErrUsenetSegmentMissing,
			checkIDs: segmentIDs(total, func(i int) bool { return i == 0 || i == total-1 }),
			wholeIDs: []string{"<s1@gate>"},
		},
		{
			name: "connection errors only", reply: statDownReply, served: all,
			checkIDs: segmentIDs(total, func(i int) bool { return i == 0 || i == total-1 }),
			wholeIDs: segmentIDs(total, all),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newPinRig(t, tc.reply)
			storeSegmented(t, rig, total, tc.served)
			err := rig.u.CheckFile(t.Context(), "gate", "Movie.mkv")
			if err != tc.wantCheck {
				t.Errorf("CheckFile = %v, want %v", err, tc.wantCheck)
			}
			if got := rig.probes.sorted("STAT"); !slices.Equal(got, tc.checkIDs) {
				t.Errorf("CheckFile probed %v, want %v", got, tc.checkIDs)
			}
			rig2 := newPinRig(t, tc.reply)
			storeSegmented(t, rig2, total, tc.served)
			err = rig2.u.CheckWholeFile(t.Context(), "gate", "Movie.mkv")
			if err != tc.wantWhole {
				t.Errorf("CheckWholeFile = %v, want %v", err, tc.wantWhole)
			}
			got := rig2.probes.sorted("STAT")
			whole := tc.wantWhole == nil && slices.Equal(got, tc.wholeIDs)
			contains := tc.wantWhole != nil && !slices.ContainsFunc(tc.wholeIDs, func(id string) bool { return !slices.Contains(got, id) })
			if !whole && !contains {
				t.Errorf("CheckWholeFile probed %v, want %v (all of them without a miss, at least these with one)", got, tc.wholeIDs)
			}
		})
	}
}

type observedSource struct {
	parser.ArticleSource
	observed map[string]bool
}

func (s observedSource) IsAvailable(messageID string) bool { return s.observed[messageID] }

func TestPinFilterUnobserved(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) *pinRig {
		rig := newPinRig(t, nil)
		rig.u.analyzer = parser.NewParserWithSource(
			observedSource{observed: map[string]bool{"<s2@gate>": true}}, 1, zerolog.Nop())
		storeSegmented(t, rig, 2, func(i int) bool { return i == 0 })
		return rig
	}
	t.Run("CheckFile skips the observed id", func(t *testing.T) {
		t.Parallel()
		rig := setup(t)
		if err := rig.u.CheckFile(t.Context(), "gate", "Movie.mkv"); err != nil {
			t.Fatalf("CheckFile = %v, want nil", err)
		}
		if got, want := rig.probes.sorted("STAT"), []string{"<s1@gate>"}; !slices.Equal(got, want) {
			t.Fatalf("probed %v, want %v", got, want)
		}
	})
	t.Run("CheckWholeFile probes the observed id", func(t *testing.T) {
		t.Parallel()
		rig := setup(t)
		if err := rig.u.CheckWholeFile(t.Context(), "gate", "Movie.mkv"); err != customerror.ErrUsenetSegmentMissing {
			t.Fatalf("CheckWholeFile = %v, want the missing sentinel", err)
		}
		if got, want := rig.probes.sorted("STAT"), []string{"<s1@gate>", "<s2@gate>"}; !slices.Equal(got, want) {
			t.Fatalf("probed %v, want %v", got, want)
		}
	})
	t.Run("the import gate skips the observed id", func(t *testing.T) {
		t.Parallel()
		rig := setup(t)
		nzb := &storage.NZB{ID: "gate", Files: []storage.NZBFile{segmentedFile(2)}}
		missing, err := rig.u.checkNZBAvailability(t.Context(), nzb)
		if missing || err != nil {
			t.Fatalf("checkNZBAvailability = %v, %v, want a pass", missing, err)
		}
		if got, want := rig.probes.sorted("STAT"), []string{"<s1@gate>"}; !slices.Equal(got, want) {
			t.Fatalf("probed %v, want %v", got, want)
		}
	})
}

func TestPinManifestErrorsBeforeStat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		store func(t *testing.T, rig *pinRig)
		file  string
		want  string
		is    error
	}{
		{
			name: "no such NZB", store: func(*testing.T, *pinRig) {}, file: "Movie.mkv",
			want: "failed to sample file segments",
		},
		{
			name: "file has no segments", file: "Movie.mkv",
			store: func(t *testing.T, rig *pinRig) {
				file := gateFile(10)
				file.Segments = nil
				gateNZB(t, rig.u, file)
			},
			want: "file has no Segments: Movie.mkv", is: customerror.ErrUsenetManifestInvalid,
		},
		{
			name: "no such file", file: "Other.mkv",
			store: func(t *testing.T, rig *pinRig) { gateNZB(t, rig.u, gateFile(10)) },
			want:  "file has no Segments: Other.mkv", is: customerror.ErrUsenetManifestInvalid,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for name, call := range map[string]func(*Usenet) error{
				"CheckFile":      func(u *Usenet) error { return u.CheckFile(t.Context(), "gate", tc.file) },
				"CheckWholeFile": func(u *Usenet) error { return u.CheckWholeFile(t.Context(), "gate", tc.file) },
			} {
				rig := newPinRig(t, nil)
				tc.store(t, rig)
				err := call(rig.u)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s = %v, want it to contain %q", name, err, tc.want)
				}
				if tc.is != nil && !errors.Is(err, tc.is) {
					t.Errorf("%s = %v, want it to wrap %v", name, err, tc.is)
				}
				if got := rig.probes.sorted("STAT"); len(got) != 0 {
					t.Errorf("%s probed %v before failing", name, got)
				}
			}
		})
	}
}

type failingBodySource struct {
	parser.ArticleSource
	err error
}

func (s failingBodySource) Body(context.Context, string) ([]byte, error) { return nil, s.err }
func (failingBodySource) IsAvailable(string) bool                        { return false }
func (failingBodySource) Metrics() parser.ArticleMetrics                 { return parser.ArticleMetrics{} }

func TestPinReaderErrorWrappingASentinelFailsTheNZBAtHead(t *testing.T) {
	t.Parallel()
	readErr := fmt.Errorf("read failed: %w", customerror.ErrUsenetCorruptContent)
	rig := newPinRig(t, nil)
	rig.u.analyzer = parser.NewParserWithSource(failingBodySource{err: readErr}, 1, zerolog.Nop())
	rig.serve(t, mkvPayload)
	nzb := gateNZB(t, rig.u, gateFile(len(mkvPayload)))

	got, err := rig.u.finish(t.Context(), nzb)
	if err == nil || !errors.Is(err, customerror.ErrUsenetCorruptContent) ||
		!strings.HasPrefix(err.Error(), "content verification failed: ") {
		t.Fatalf("finish error = %v, want the content gate to fail the NZB", err)
	}
	if got != (ProcessResult{}) {
		t.Fatalf("ProcessResult = %+v, want the zero value", got)
	}
	if nzb.Status != NZBStatusFailed || !strings.Contains(nzb.FailMessage, "read failed") {
		t.Fatalf("Status = %q, FailMessage = %q, want a recorded failure", nzb.Status, nzb.FailMessage)
	}
	rig.assertWarns(t, map[string]int{warnContentFailed: 1})

	file := gateFile(len(mkvPayload))
	verifyErr := rig.u.VerifyFileHead(t.Context(), &file)
	if !errors.Is(verifyErr, customerror.ErrUsenetCorruptContent) || !strings.Contains(verifyErr.Error(), "read failed") {
		t.Fatalf("VerifyFileHead = %v, want the reader error with its chain", verifyErr)
	}
}
