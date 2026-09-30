package nntp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
)

// ErrorType classifies NNTP operation failures.
type ErrorType int

// Error types for NNTP operations.
const (
	ErrorTypeUnknown ErrorType = iota
	ErrorTypeConnection
	ErrorTypeAuthentication
	ErrorTypeTimeout
	ErrorTypeArticleNotFound
	ErrorTypeGroupNotFound
	ErrorTypePermissionDenied
	ErrorTypeServerBusy
	ErrorTypeInvalidCommand
	ErrorTypeProtocol
	ErrorTypeYencDecode
)

// NNTP response codes (RFC 3977, RFC 4643).
const (
	codeDate               = 111
	codeBodyFollows        = 222
	codeArticleExists      = 223
	codeAuthAccepted       = 281
	codePasswordRequired   = 381
	codeServiceUnavailable = 400
	codeNoSuchGroup        = 411
	codeNoArticleWithNum   = 423
	codeNoSuchArticle      = 430
	codeAuthRejected       = 481
	codeAuthOutOfSequence  = 482
	codeUnknownCommand     = 500
	codeSyntaxError        = 501
	codePermissionDenied   = 502
	codeServiceDown        = 503
)

// Error represents an NNTP-specific error.
type Error struct {
	Type    ErrorType
	Code    int    // NNTP response code
	Message string // Error message
	Err     error  // Underlying error
}

// NewConnectionError wraps a transport failure.
func NewConnectionError(err error) *Error {
	return &Error{
		Type:    ErrorTypeConnection,
		Message: "connection failed",
		Err:     err,
	}
}

// NewTimeoutError wraps a deadline failure.
func NewTimeoutError(err error) *Error {
	return &Error{
		Type:    ErrorTypeTimeout,
		Message: "operation timed out",
		Err:     err,
	}
}

// NewProtocolError reports a response the client could not interpret.
func NewProtocolError(code int, message string) *Error {
	return &Error{
		Type:    ErrorTypeProtocol,
		Code:    code,
		Message: message,
	}
}

// NewYencDecodeError reports a body that failed yEnc decoding or CRC checks.
func NewYencDecodeError(err error) *Error {
	return &Error{
		Type:    ErrorTypeYencDecode,
		Message: "yEnc decode failed",
		Err:     err,
	}
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("NNTP %s (code %d): %s - %v", e.Type.String(), e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("NNTP %s (code %d): %s", e.Type.String(), e.Code, e.Message)
}

func (e *Error) Unwrap() error {
	return e.Err
}

// Is matches any *Error of the same Type.
func (e *Error) Is(target error) bool {
	if t, ok := errors.AsType[*Error](target); ok {
		return e.Type == t.Type
	}
	return false
}

// IsRetryable returns true if the error might be resolved by retrying.
func (e *Error) IsRetryable() bool {
	return e.Type == ErrorTypeConnection || e.Type == ErrorTypeTimeout || e.Type == ErrorTypeServerBusy
}

func (et ErrorType) String() string {
	names := [...]string{
		ErrorTypeUnknown:          "UNKNOWN",
		ErrorTypeConnection:       "CONNECTION",
		ErrorTypeAuthentication:   "AUTHENTICATION",
		ErrorTypeTimeout:          "TIMEOUT",
		ErrorTypeArticleNotFound:  "ARTICLE_NOT_FOUND",
		ErrorTypeGroupNotFound:    "GROUP_NOT_FOUND",
		ErrorTypePermissionDenied: "PERMISSION_DENIED",
		ErrorTypeServerBusy:       "SERVER_BUSY",
		ErrorTypeInvalidCommand:   "INVALID_COMMAND",
		ErrorTypeProtocol:         "PROTOCOL",
		ErrorTypeYencDecode:       "YENC_DECODE",
	}
	if et < 0 || int(et) >= len(names) {
		return names[ErrorTypeUnknown]
	}
	return names[et]
}

func classifyTransferError(message string, err error) *Error {
	wrapped := fmt.Errorf("%s: %w", message, err)
	switch {
	case isTimeoutLike(err):
		return NewTimeoutError(wrapped)
	case isConnectionLike(err):
		return NewConnectionError(wrapped)
	default:
		return NewYencDecodeError(wrapped)
	}
}

func isTimeoutLike(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func isConnectionLike(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "unexpected eof")
}

// classifyNNTPError classifies an NNTP response code into an error type.
func classifyNNTPError(code int, message string) *Error {
	var typ ErrorType
	switch code {
	case codeNoSuchArticle, codeNoArticleWithNum:
		typ = ErrorTypeArticleNotFound
	case codeNoSuchGroup:
		typ = ErrorTypeGroupNotFound
	case codePermissionDenied:
		typ = ErrorTypePermissionDenied
	case codeServiceUnavailable, codeServiceDown:
		// 503 = "Service temporarily unavailable" — transient, not a permission failure
		typ = ErrorTypeServerBusy
	case codeAuthRejected, codeAuthOutOfSequence:
		typ = ErrorTypeAuthentication
	case codeUnknownCommand, codeSyntaxError:
		typ = ErrorTypeInvalidCommand
	default:
		if code >= codeServiceUnavailable {
			typ = ErrorTypeProtocol
		}
	}
	return &Error{Type: typ, Code: code, Message: message}
}

// IsArticleNotFoundError reports whether err is a missing-article failure.
func IsArticleNotFoundError(err error) bool {
	if nntpErr, ok := errors.AsType[*Error](err); ok {
		return nntpErr.Type == ErrorTypeArticleNotFound
	}
	return false
}

// IsYencDecodeError reports definitive article corruption after decoding
// (including CRC mismatch). Callers should try other backbones, but should
// not multiply retries against the same exhausted provider set.
func IsYencDecodeError(err error) bool {
	var nntpErr *Error
	return errors.As(err, &nntpErr) && nntpErr.Type == ErrorTypeYencDecode
}
