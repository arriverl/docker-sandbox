// Package sandboxerr 提供 Docker 沙箱客户端的统一错误分类与包装。
package sandboxerr

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

// Kind 表示可程序化识别的错误类别。
type Kind string

const (
	KindUnknown      Kind = "unknown"
	KindUnavailable  Kind = "unavailable"
	KindTimeout      Kind = "timeout"
	KindNotFound     Kind = "not_found"
	KindConflict     Kind = "conflict"
	KindUnauthorized Kind = "unauthorized"
	KindForbidden    Kind = "forbidden"
	KindInvalid      Kind = "invalid"
	KindCanceled     Kind = "canceled"
)

// Sentinel errors for errors.Is matching on kind.
var (
	ErrUnavailable  = errors.New("docker daemon unavailable")
	ErrTimeout      = errors.New("docker request timed out")
	ErrNotFound     = errors.New("docker resource not found")
	ErrConflict     = errors.New("docker resource conflict")
	ErrUnauthorized = errors.New("docker unauthorized")
	ErrForbidden    = errors.New("docker forbidden")
	ErrInvalid      = errors.New("invalid docker client configuration")
	ErrCanceled     = errors.New("docker request canceled")
)

// Error 是带操作名与分类的包装错误。
type Error struct {
	Op   string
	Kind Kind
	Err  error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Op == "" {
		return fmt.Sprintf("%s: %v", e.Kind, e.Err)
	}
	return fmt.Sprintf("%s: %s: %v", e.Op, e.Kind, e.Err)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *Error) Is(target error) bool {
	if e == nil {
		return false
	}
	switch target {
	case ErrUnavailable:
		return e.Kind == KindUnavailable
	case ErrTimeout:
		return e.Kind == KindTimeout
	case ErrNotFound:
		return e.Kind == KindNotFound
	case ErrConflict:
		return e.Kind == KindConflict
	case ErrUnauthorized:
		return e.Kind == KindUnauthorized
	case ErrForbidden:
		return e.Kind == KindForbidden
	case ErrInvalid:
		return e.Kind == KindInvalid
	case ErrCanceled:
		return e.Kind == KindCanceled
	default:
		return false
	}
}

// Wrap 为底层错误附加操作名并自动分类。nil 原样返回。
func Wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	var se *Error
	if errors.As(err, &se) {
		if se.Op == "" {
			se.Op = op
		}
		return se
	}
	return &Error{Op: op, Kind: Classify(err), Err: err}
}

// WrapKind 使用指定分类包装错误。
func WrapKind(op string, kind Kind, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Op: op, Kind: kind, Err: err}
}

// Classify 将任意错误映射为 Kind。
func Classify(err error) Kind {
	if err == nil {
		return KindUnknown
	}
	var se *Error
	if errors.As(err, &se) {
		return se.Kind
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, ErrCanceled):
		return KindCanceled
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrTimeout):
		return KindTimeout
	case errors.Is(err, ErrInvalid):
		return KindInvalid
	case errors.Is(err, ErrNotFound):
		return KindNotFound
	case errors.Is(err, ErrConflict):
		return KindConflict
	case errors.Is(err, ErrUnauthorized):
		return KindUnauthorized
	case errors.Is(err, ErrForbidden):
		return KindForbidden
	case errors.Is(err, ErrUnavailable):
		return KindUnavailable
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return KindTimeout
	}
	if isUnavailable(err) {
		return KindUnavailable
	}
	return KindUnknown
}

func isUnavailable(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, os.ErrNotExist) {
		return true
	}
	msg := strings.ToLower(err.Error())
	needles := []string{
		"cannot connect to the docker daemon",
		"connection refused",
		"connectex",
		"no such file or directory",
		"the system cannot find the file",
		"no connection could be made",
		"actively refused",
		"error during connect",
		"named pipe",
		"npipe",
		"daemon is not running",
		"is the docker daemon running",
	}
	for _, n := range needles {
		if strings.Contains(msg, n) {
			return true
		}
	}
	return false
}

func IsUnavailable(err error) bool  { return Classify(err) == KindUnavailable }
func IsTimeout(err error) bool      { return Classify(err) == KindTimeout }
func IsNotFound(err error) bool     { return Classify(err) == KindNotFound }
func IsConflict(err error) bool     { return Classify(err) == KindConflict }
func IsUnauthorized(err error) bool { return Classify(err) == KindUnauthorized }
func IsForbidden(err error) bool    { return Classify(err) == KindForbidden }
func IsInvalid(err error) bool      { return Classify(err) == KindInvalid }
func IsCanceled(err error) bool     { return Classify(err) == KindCanceled }
