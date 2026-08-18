package dockerx_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gitproduct/docker-sandbox/dockerx"
	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func TestRenderDockerfile_FromBaseEnvLabelStepsInject(t *testing.T) {
	got, err := dockerx.RenderDockerfile(dockerx.BuildSpec{
		BaseImage: "alpine:3.20",
		Env:       map[string]string{"FOO": "bar"},
		Labels:    map[string]string{"app": "sandbox"},
		Steps:     []string{"RUN echo hi"},
		Inject:    []dockerx.InjectFile{{Path: "/app/main.py", Content: []byte("print(1)")}},
		Workdir:   "/app",
		Cmd:       []string{"python", "main.py"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"FROM alpine:3.20",
		`ENV FOO="bar"`,
		`LABEL app="sandbox"`,
		"COPY app/main.py /app/main.py",
		"RUN echo hi",
		"WORKDIR /app",
		`CMD ["python","main.py"]`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
}

func TestRenderDockerfile_CustomDockerfile(t *testing.T) {
	got, err := dockerx.RenderDockerfile(dockerx.BuildSpec{
		Dockerfile: "FROM busybox\nRUN true",
		Inject:     []dockerx.InjectFile{{Path: "/etc/note", Content: []byte("x")}},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(got, "FROM busybox") || !strings.Contains(got, "COPY etc/note /etc/note") {
		t.Fatalf("got %s", got)
	}
}

func TestRenderDockerfile_PrependsFromWhenMissing(t *testing.T) {
	got, err := dockerx.RenderDockerfile(dockerx.BuildSpec{
		BaseImage:  "alpine:3.20",
		Dockerfile: "RUN echo from-base",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(got), "FROM alpine:3.20") {
		t.Fatalf("got %s", got)
	}
}

func TestBuild_RequiresTagAndBase(t *testing.T) {
	b := dockerx.NewImageBuilder(dockerx.NewWithAPI(&fakeEngine{}))
	_, err := b.Build(context.Background(), dockerx.BuildSpec{BaseImage: "alpine"})
	if !sandboxerr.IsInvalid(err) || !strings.Contains(err.Error(), "标签") {
		t.Fatalf("tag err = %v", err)
	}
	_, err = b.Build(context.Background(), dockerx.BuildSpec{Tag: "demo:1"})
	if !sandboxerr.IsInvalid(err) || !strings.Contains(err.Error(), "基础镜像") {
		t.Fatalf("base err = %v", err)
	}
}

func TestBuild_MissingInjectFileInChinese(t *testing.T) {
	b := dockerx.NewImageBuilder(dockerx.NewWithAPI(&fakeEngine{}))
	_, err := b.Build(context.Background(), dockerx.BuildSpec{
		Tag:       "sandbox/app:1",
		BaseImage: "alpine:3.20",
		Inject:    []dockerx.InjectFile{{Path: "/app/app.py", Source: `.\app.py`}},
	})
	if !sandboxerr.IsNotFound(err) {
		t.Fatalf("err = %v, want not_found", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "注入文件不存在") || !strings.Contains(msg, "app.py") {
		t.Fatalf("want missing-file context, got %q", msg)
	}
}

func TestSplitInjectSpec_WindowsAndUnix(t *testing.T) {
	src, dest, err := dockerx.SplitInjectSpec(`.\app.py:/app/app.py`)
	if err != nil || src != `.\app.py` || dest != "/app/app.py" {
		t.Fatalf("unix-style = %q %q %v", src, dest, err)
	}
	src, dest, err = dockerx.SplitInjectSpec(`C:\tmp\app.py:/app/app.py`)
	if err != nil || src != `C:\tmp\app.py` || dest != "/app/app.py" {
		t.Fatalf("windows = %q %q %v", src, dest, err)
	}
}

func TestBuild_RejectsPathTraversal(t *testing.T) {
	b := dockerx.NewImageBuilder(dockerx.NewWithAPI(&fakeEngine{}))
	_, err := b.Build(context.Background(), dockerx.BuildSpec{
		Tag:       "demo:1",
		BaseImage: "alpine",
		Inject:    []dockerx.InjectFile{{Path: "../etc/passwd", Content: []byte("x")}},
	})
	if !sandboxerr.IsInvalid(err) || !strings.Contains(err.Error(), "注入") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuild_SendsContextAndReturnsID(t *testing.T) {
	fake := &fakeEngine{buildID: "sha256:abc"}
	b := dockerx.NewImageBuilder(dockerx.NewWithAPI(fake))
	got, err := b.Build(context.Background(), dockerx.BuildSpec{
		Tag:       "sandbox/demo:1",
		BaseImage: "alpine:3.20",
		Env:       map[string]string{"A": "1"},
		Inject:    []dockerx.InjectFile{{Path: "/opt/hi.txt", Content: []byte("hello")}},
		Steps:     []string{"RUN true"},
		Labels:    map[string]string{"k": "v"},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got.ID != "sha256:abc" || got.Tag != "sandbox/demo:1" {
		t.Fatalf("result = %+v", got)
	}
	if fake.lastBuildDockerfile == "" || !strings.Contains(fake.lastBuildDockerfile, "FROM alpine:3.20") {
		t.Fatalf("dockerfile = %s", fake.lastBuildDockerfile)
	}
	if fake.lastBuildFiles["opt/hi.txt"] != "hello" {
		t.Fatalf("files = %#v", fake.lastBuildFiles)
	}
	if fake.lastBuildTags[0] != "sandbox/demo:1" {
		t.Fatalf("tags = %v", fake.lastBuildTags)
	}
}

func TestBuild_StreamErrorInChinese(t *testing.T) {
	fake := &fakeEngine{buildStream: `{"error":"The command '/bin/sh -c false' returned a non-zero code: 1"}` + "\n"}
	b := dockerx.NewImageBuilder(dockerx.NewWithAPI(fake))
	_, err := b.Build(context.Background(), dockerx.BuildSpec{Tag: "demo:1", BaseImage: "alpine"})
	if err == nil {
		t.Fatal("want build error")
	}
	if !strings.Contains(err.Error(), "构建") || !strings.Contains(err.Error(), "non-zero") {
		t.Fatalf("err = %q", err)
	}
}
