package sandbox

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func TestStdHealthHTTPAndTCP(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ready" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer httpServer.Close()
	_, portText, err := net.SplitHostPort(httpServer.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	httpPort, _ := strconv.Atoi(portText)
	health := NewStdHealth(nil)
	inst := Instance{HostPorts: []int{httpPort}}
	if err := health.Check(context.Background(), inst, Probe{Kind: ProbeHTTP, Path: "ready"}); err != nil {
		t.Fatalf("HTTP ready: %v", err)
	}
	if err := health.Check(context.Background(), inst, Probe{Kind: ProbeHTTP, Path: "/pending"}); !IsProbeNotReady(err) {
		t.Fatalf("HTTP pending err=%v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	tcpPort := listener.Addr().(*net.TCPAddr).Port
	if err := health.Check(context.Background(), Instance{}, Probe{Kind: ProbeTCP, Port: tcpPort}); err != nil {
		t.Fatalf("TCP ready: %v", err)
	}
}

func TestStdHealthExecClassifiesNotReadyAndPermanentErrors(t *testing.T) {
	calls := 0
	health := NewStdHealth(func(context.Context, string, []string) error {
		calls++
		if calls == 1 {
			return sandboxerr.WrapKind("exec", sandboxerr.KindInvalid, errors.New("exit 1"))
		}
		return nil
	})
	inst := Instance{ContainerID: "cid"}
	probe := Probe{Kind: ProbeExec, Command: []string{"true"}}
	if err := health.Check(context.Background(), inst, probe); !IsProbeNotReady(err) {
		t.Fatalf("exec pending err=%v", err)
	}
	if err := health.Check(context.Background(), inst, probe); err != nil {
		t.Fatalf("exec ready: %v", err)
	}
	if err := (&StdHealth{}).Check(context.Background(), inst, probe); !sandboxerr.IsInvalid(err) {
		t.Fatalf("missing exec err=%v", err)
	}
	var nilHealth *StdHealth
	if err := nilHealth.Check(context.Background(), inst, probe); !sandboxerr.IsInvalid(err) {
		t.Fatalf("nil health err=%v", err)
	}
}

func TestProbeNotReadyNil(t *testing.T) {
	if ProbeNotReady(nil) != nil {
		t.Fatal("ProbeNotReady(nil) should be nil")
	}
}

func TestValidateProbeRejectsInvalidConfiguration(t *testing.T) {
	inst := Instance{ID: "box1", Status: StatusRunning, ContainerID: "cid"}
	tests := []struct {
		name  string
		probe Probe
	}{
		{name: "unknown kind", probe: Probe{Kind: ProbeKind("udp")}},
		{name: "http without port", probe: Probe{Kind: ProbeHTTP}},
		{name: "tcp without port", probe: Probe{Kind: ProbeTCP}},
		{name: "exec without command", probe: Probe{Kind: ProbeExec}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateProbe(inst, tt.probe); !sandboxerr.IsInvalid(err) {
				t.Fatalf("ValidateProbe err=%v want invalid", err)
			}
		})
	}
}

func TestWaitReadyReturnsPermanentProbeErrorImmediately(t *testing.T) {
	world := newMemWorld()
	svc := testService(t, world)
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(context.Background(), "box1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()

	_, err := svc.WaitReady(ctx, "box1", Probe{Kind: ProbeKind("udp")})
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("WaitReady err=%v want invalid", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("permanent probe error returned after %v", elapsed)
	}
}

func TestWaitReadyRetriesUnknownHealthFailure(t *testing.T) {
	world := newMemWorld()
	world.health = func(_ Instance, _ Probe, n int) error {
		if n < 2 {
			return errProbePending
		}
		return nil
	}
	svc := testService(t, world)
	if _, err := svc.Create(context.Background(), sampleSpec()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(context.Background(), "box1"); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.WaitReady(context.Background(), "box1", Probe{Kind: ProbeTCP, Port: 12345}); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

var errProbePending = sandboxerr.WrapKind("probe", sandboxerr.KindUnknown, context.DeadlineExceeded)
