package sandboxerr_test

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func TestClassify_LocalMissingFileIsNotDockerUnavailable(t *testing.T) {
	if got := sandboxerr.Classify(os.ErrNotExist); got == sandboxerr.KindUnavailable {
		t.Fatalf("Classify(os.ErrNotExist)=%s want non-unavailable", got)
	}
	pathErr := &os.PathError{Op: "open", Path: "/tmp/missing", Err: os.ErrNotExist}
	if got := sandboxerr.Classify(pathErr); got == sandboxerr.KindUnavailable {
		t.Fatalf("Classify(PathError)=%s want non-unavailable", got)
	}
}

func TestWrapKind_NilAndSentinels(t *testing.T) {
	if err := sandboxerr.WrapKind("op", sandboxerr.KindNotFound, nil); err != nil {
		t.Fatalf("WrapKind(nil)=%v", err)
	}
	err := sandboxerr.WrapKind("get", sandboxerr.KindNotFound, sandboxerr.ErrNotFound)
	if !sandboxerr.IsNotFound(err) {
		t.Fatalf("IsNotFound=%v", err)
	}
	conflict := sandboxerr.WrapKind("create", sandboxerr.KindConflict, sandboxerr.ErrConflict)
	if !sandboxerr.IsConflict(conflict) {
		t.Fatalf("IsConflict=%v", conflict)
	}
	unauth := sandboxerr.WrapKind("auth", sandboxerr.KindUnauthorized, sandboxerr.ErrUnauthorized)
	if !sandboxerr.IsUnauthorized(unauth) {
		t.Fatalf("IsUnauthorized=%v", unauth)
	}
	forbid := sandboxerr.WrapKind("auth", sandboxerr.KindForbidden, sandboxerr.ErrForbidden)
	if !sandboxerr.IsForbidden(forbid) {
		t.Fatalf("IsForbidden=%v", forbid)
	}
}

func TestWrap_NilReturnsNil(t *testing.T) {
	if err := sandboxerr.Wrap("ping", nil); err != nil {
		t.Fatalf("Wrap(nil) = %v, want nil", err)
	}
}

func TestWrap_AnnotatesOpAndClassifiesTimeout(t *testing.T) {
	err := sandboxerr.Wrap("info", context.DeadlineExceeded)

	var se *sandboxerr.Error
	if !errors.As(err, &se) {
		t.Fatalf("got %T, want *sandboxerr.Error", err)
	}
	if se.Op != "info" {
		t.Fatalf("Op = %q, want info", se.Op)
	}
	if se.Kind != sandboxerr.KindTimeout {
		t.Fatalf("Kind = %q, want %q", se.Kind, sandboxerr.KindTimeout)
	}
	if !sandboxerr.IsTimeout(err) {
		t.Fatal("expected IsTimeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expected unwrap to context.DeadlineExceeded")
	}
}

func TestClassify_Canceled(t *testing.T) {
	if got := sandboxerr.Classify(context.Canceled); got != sandboxerr.KindCanceled {
		t.Fatalf("Classify(Canceled) = %q, want %q", got, sandboxerr.KindCanceled)
	}
	if !sandboxerr.IsCanceled(sandboxerr.Wrap("ping", context.Canceled)) {
		t.Fatal("expected IsCanceled")
	}
}

func TestClassify_ConnectionRefusedIsUnavailable(t *testing.T) {
	err := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	wrapped := sandboxerr.Wrap("ping", err)
	if !sandboxerr.IsUnavailable(wrapped) {
		t.Fatalf("Kind = %q, want unavailable", sandboxerr.Classify(wrapped))
	}
}

func TestClassify_DaemonMessageIsUnavailable(t *testing.T) {
	err := errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")
	if got := sandboxerr.Classify(err); got != sandboxerr.KindUnavailable {
		t.Fatalf("Classify(daemon msg) = %q, want %q", got, sandboxerr.KindUnavailable)
	}
}

func TestClassify_WindowsPipeMissingIsUnavailable(t *testing.T) {
	err := errors.New(`error during connect: Get "http://%2F%2F.%2Fpipe%2Fdocker_engine/_ping": open //./pipe/docker_engine: The system cannot find the file specified.`)
	if !sandboxerr.IsUnavailable(err) {
		t.Fatalf("Classify(npipe missing) = %q, want unavailable", sandboxerr.Classify(err))
	}
}

func TestClassify_InvalidConfig(t *testing.T) {
	err := sandboxerr.Wrap("config", sandboxerr.ErrInvalid)
	if !sandboxerr.IsInvalid(err) {
		t.Fatal("expected IsInvalid")
	}
}

func TestActionOf_DestroyedCreateAndMaxInstances(t *testing.T) {
	destroyed := sandboxerr.WrapKindAction("instance.create", sandboxerr.KindConflict, sandboxerr.ActionUseRestore, sandboxerr.ErrConflict)
	if sandboxerr.ActionOf(destroyed) != sandboxerr.ActionUseRestore {
		t.Fatalf("ActionOf(destroyed)=%q", sandboxerr.ActionOf(destroyed))
	}
	wrapped := sandboxerr.WrapKind("创建实例", sandboxerr.KindConflict, destroyed)
	if sandboxerr.ActionOf(wrapped) != sandboxerr.ActionUseRestore {
		t.Fatalf("WrapKind must keep action, got %q", sandboxerr.ActionOf(wrapped))
	}
	capped := sandboxerr.WrapKindAction("instance.create", sandboxerr.KindConflict, sandboxerr.ActionRetryAfter, sandboxerr.ErrConflict)
	if sandboxerr.ActionOf(capped) != sandboxerr.ActionRetryAfter {
		t.Fatalf("ActionOf(capped)=%q", sandboxerr.ActionOf(capped))
	}
	if sandboxerr.ActionOf(errors.New("plain")) != "" {
		t.Fatal("plain error should have empty action")
	}
}

func TestError_ErrorStringIncludesOpAndKind(t *testing.T) {
	err := sandboxerr.Wrap("version", context.DeadlineExceeded)
	got := err.Error()
	if got == "" {
		t.Fatal("empty error string")
	}
	for _, want := range []string{"version", string(sandboxerr.KindTimeout)} {
		if !strings.Contains(got, want) {
			t.Fatalf("Error() = %q, want substring %q", got, want)
		}
	}
}
