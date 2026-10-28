package dockerx_test

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/gitproduct/docker-sandbox/dockerx"
	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func TestNew_InvalidHost(t *testing.T) {
	_, err := dockerx.New(dockerx.Config{Host: "not-a-host"})
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("New(invalid host) err = %v, want invalid", err)
	}
}

func TestNew_InvalidProxyURL(t *testing.T) {
	_, err := dockerx.New(dockerx.Config{
		Host:     "tcp://127.0.0.1:2375",
		ProxyURL: "://bad-proxy",
	})
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("New(invalid proxy) err = %v, want invalid", err)
	}
}

func TestNew_ProxyRejectedForUnixHost(t *testing.T) {
	_, err := dockerx.New(dockerx.Config{
		Host:     "unix:///var/run/docker.sock",
		ProxyURL: "http://127.0.0.1:8888",
	})
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("New(proxy+unix) err = %v, want invalid", err)
	}
}

func TestNew_DisableProxyConflictsWithProxyURL(t *testing.T) {
	_, err := dockerx.New(dockerx.Config{
		Host:         "tcp://127.0.0.1:2375",
		ProxyURL:     "http://127.0.0.1:8888",
		DisableProxy: true,
	})
	if !sandboxerr.IsInvalid(err) {
		t.Fatalf("New(disable+proxy) err = %v, want invalid", err)
	}
}

func TestConfig_HTTPClientDisableProxyIgnoresEnv(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("http_proxy", "http://127.0.0.1:9")
	t.Setenv("https_proxy", "http://127.0.0.1:9")

	cfg := dockerx.Config{
		Host:         "tcp://127.0.0.1:2375",
		DisableProxy: true,
		Timeout:      5 * time.Second,
	}
	assertNoProxy(t, cfg)
}

func TestConfig_LocalHostIgnoresEnvProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")

	cfg := dockerx.Config{Host: "npipe:////./pipe/docker_engine"}
	assertNoProxy(t, cfg)
}

func assertNoProxy(t *testing.T, cfg dockerx.Config) {
	t.Helper()
	hc, err := cfg.HTTPClient()
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type %T, want *http.Transport", hc.Transport)
	}
	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy: %v", err)
	}
	if proxy != nil {
		t.Fatalf("proxy = %v, want nil (disabled)", proxy)
	}
}

func TestClient_NewSucceedsWhenDaemonDown(t *testing.T) {
	c, err := dockerx.New(dockerx.Config{
		Host:         "tcp://127.0.0.1:1",
		Timeout:      2 * time.Second,
		DisableProxy: true,
	})
	if err != nil {
		t.Fatalf("New should not require a running daemon, got %v", err)
	}
	defer c.Close()
}

func TestClient_PingUnreachableDaemonIsUnavailable(t *testing.T) {
	c, err := dockerx.New(dockerx.Config{
		Host:         "tcp://127.0.0.1:1",
		Timeout:      2 * time.Second,
		DisableProxy: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	_, err = c.Ping(context.Background())
	if err == nil {
		t.Fatal("expected ping error when daemon is unreachable")
	}
	if !sandboxerr.IsUnavailable(err) && !sandboxerr.IsTimeout(err) {
		t.Fatalf("Ping err = %v (kind %q), want unavailable or timeout", err, sandboxerr.Classify(err))
	}
}

func TestConfig_HTTPClientDoesNotForceHTTP2(t *testing.T) {
	cfg := dockerx.Config{
		Host:         "unix:///var/run/docker.sock",
		DisableProxy: true,
		Timeout:      5 * time.Second,
	}
	hc, err := cfg.HTTPClient()
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type %T, want *http.Transport", hc.Transport)
	}
	if tr.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 must stay false so WithHost can use HTTP/1.1 on unix/npipe")
	}
	if tr.DialContext != nil {
		t.Fatal("DialContext must be unset so client.WithHost can install the unix/npipe dialer")
	}
}

func TestConfig_HTTPClientUsesProxy(t *testing.T) {
	cfg := dockerx.Config{
		Host:     "tcp://127.0.0.1:2375",
		ProxyURL: "http://127.0.0.1:8888",
		Timeout:  5 * time.Second,
	}
	hc, err := cfg.HTTPClient()
	if err != nil {
		t.Fatalf("HTTPClient: %v", err)
	}
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type %T, want *http.Transport", hc.Transport)
	}
	req, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy: %v", err)
	}
	if proxy == nil || proxy.String() != "http://127.0.0.1:8888" {
		t.Fatalf("proxy = %v, want http://127.0.0.1:8888", proxy)
	}
	if hc.Timeout != 5*time.Second {
		t.Fatalf("Timeout = %s, want 5s", hc.Timeout)
	}
}

type fakeEngine struct {
	host                string
	ping                types.Ping
	ver                 types.Version
	info                system.Info
	containers          []container.Summary
	hideCreatedFromList bool
	images              []image.Summary
	err                 error
	ops                 []string
	createID            string
	createWarnings      []string
	createErrOnce       error
	lastName            string
	lastConfig          *container.Config
	lastHost            *container.HostConfig
	lastStop            container.StopOptions
	lastRemove          container.RemoveOptions
	inspect             container.InspectResponse
	imageMissing        bool
	pullErr             error
	pulled              map[string]bool
	pullStream          string
	pullHold            <-chan struct{}
	pullEntered         chan struct{}
	mu                  sync.Mutex
	state               container.ContainerState
	gone                bool
	networks            map[string]network.Inspect
	volumes             map[string]volume.Volume
	lastBuildDockerfile string
	lastBuildFiles      map[string]string
	lastBuildTags       []string
	buildID             string
	buildStream         string
	execExit            int
	execErr             error
	lastExecCmd         []string
}

func (f *fakeEngine) Ping(context.Context) (types.Ping, error) {
	return f.ping, f.err
}
func (f *fakeEngine) ServerVersion(context.Context) (types.Version, error) {
	return f.ver, f.err
}
func (f *fakeEngine) Info(context.Context) (system.Info, error) {
	return f.info, f.err
}
func (f *fakeEngine) ContainerList(_ context.Context, opts container.ListOptions) ([]container.Summary, error) {
	if !opts.All {
		return nil, errors.New("ListContainers must request All=true")
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.lastConfig != nil && !f.gone && !f.hideCreatedFromList {
		id := f.createID
		if id == "" {
			id = "cid-1"
		}
		name := f.lastName
		if name == "" {
			name = id
		}
		st := string(f.state)
		if st == "" {
			st = string(container.StateCreated)
		}
		labels := map[string]string{}
		if f.lastConfig.Labels != nil {
			labels = f.lastConfig.Labels
		}
		return []container.Summary{{
			ID:     id,
			Names:  []string{"/" + name},
			Image:  f.lastConfig.Image,
			State:  st,
			Status: st,
			Labels: labels,
		}}, nil
	}
	return f.containers, nil
}
func (f *fakeEngine) ImageList(_ context.Context, opts image.ListOptions) ([]image.Summary, error) {
	if !opts.All {
		return nil, errors.New("ListImages must request All=true")
	}
	return f.images, f.err
}

func (f *fakeEngine) ContainerCreate(_ context.Context, config *container.Config, hostConfig *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "create")
	f.lastName = name
	f.lastConfig = config
	f.lastHost = hostConfig
	if f.createErrOnce != nil {
		err := f.createErrOnce
		f.createErrOnce = nil
		return container.CreateResponse{}, err
	}
	ref := ""
	if config != nil {
		ref = config.Image
	}
	if f.imageMissing && (f.pulled == nil || !f.pulled[ref]) {
		return container.CreateResponse{}, errdefs.NotFound(errors.New("No such image: " + ref))
	}
	if f.err != nil {
		return container.CreateResponse{}, f.err
	}
	f.gone = false
	id := f.createID
	if id == "" {
		id = "cid-1"
	}
	if hostConfig != nil {
		mode := string(hostConfig.NetworkMode)
		if mode != "" && mode != "bridge" && mode != "host" && mode != "none" && mode != "default" {
			f.attachNetwork(id, mode)
		}
	}
	return container.CreateResponse{ID: id, Warnings: f.createWarnings}, nil
}

func (f *fakeEngine) ContainerStart(_ context.Context, id string, _ container.StartOptions) error {
	f.ops = append(f.ops, "start:"+id)
	if err := f.containerOpErr(); err != nil {
		return err
	}
	f.state = container.StateRunning
	return nil
}

func (f *fakeEngine) ContainerStop(_ context.Context, id string, opts container.StopOptions) error {
	f.ops = append(f.ops, "stop:"+id)
	f.lastStop = opts
	if err := f.containerOpErr(); err != nil {
		return err
	}
	f.state = container.StateExited
	return nil
}

func (f *fakeEngine) ContainerRestart(_ context.Context, id string, opts container.StopOptions) error {
	f.ops = append(f.ops, "restart:"+id)
	f.lastStop = opts
	if err := f.containerOpErr(); err != nil {
		return err
	}
	f.state = container.StateRunning
	return nil
}

func (f *fakeEngine) ContainerRemove(_ context.Context, id string, opts container.RemoveOptions) error {
	f.ops = append(f.ops, "remove:"+id)
	f.lastRemove = opts
	if err := f.containerOpErr(); err != nil {
		return err
	}
	f.gone = true
	f.detachAllNetworks(id)
	return nil
}

func (f *fakeEngine) ContainerUnpause(_ context.Context, id string) error {
	f.ops = append(f.ops, "unpause:"+id)
	if err := f.containerOpErr(); err != nil {
		return err
	}
	f.state = container.StateRunning
	return nil
}

func (f *fakeEngine) ContainerInspect(_ context.Context, id string) (container.InspectResponse, error) {
	f.ops = append(f.ops, "inspect:"+id)
	if f.gone {
		return container.InspectResponse{}, errdefs.NotFound(errors.New("no such container: " + id))
	}
	if f.err != nil {
		return container.InspectResponse{}, f.err
	}
	if f.inspect.ContainerJSONBase != nil {
		return f.inspect, nil
	}
	st := f.state
	if st == "" {
		st = container.StateCreated
	}
	name := f.lastName
	if name == "" {
		name = id
	}
	cfg := f.lastConfig
	if cfg == nil {
		cfg = &container.Config{Image: "alpine:3.20"}
	}
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			ID:    id,
			Name:  "/" + name,
			Image: cfg.Image,
			State: &container.State{
				Status:  st,
				Running: st == container.StateRunning,
				Paused:  st == container.StatePaused,
			},
		},
		Config: cfg,
	}, nil
}

func (f *fakeEngine) containerOpErr() error {
	if f.gone {
		return errdefs.NotFound(errors.New("no such container"))
	}
	return f.err
}

func (f *fakeEngine) ImageInspectWithRaw(_ context.Context, imageID string) (image.InspectResponse, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "inspect-image:"+imageID)
	if f.imageMissing && (f.pulled == nil || !f.pulled[imageID]) {
		return image.InspectResponse{}, nil, errdefs.NotFound(errors.New("No such image: " + imageID))
	}
	return image.InspectResponse{ID: "sha256:fake"}, nil, nil
}

func (f *fakeEngine) ImagePull(_ context.Context, refStr string, _ image.PullOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	f.ops = append(f.ops, "pull:"+refStr)
	hold := f.pullHold
	entered := f.pullEntered
	stream := f.pullStream
	pullErr := f.pullErr
	f.mu.Unlock()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if hold != nil {
		<-hold
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if pullErr != nil {
		return nil, pullErr
	}
	if f.pulled == nil {
		f.pulled = map[string]bool{}
	}
	f.pulled[refStr] = true
	body := `{"status":"Pull complete"}` + "\n"
	if stream != "" {
		body = stream
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

func (f *fakeEngine) countOps(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, op := range f.ops {
		if strings.HasPrefix(op, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeEngine) ensureNetworks() {
	if f.networks == nil {
		f.networks = map[string]network.Inspect{}
	}
}

func (f *fakeEngine) attachNetwork(containerID, netName string) {
	f.ensureNetworks()
	n, ok := f.networks[netName]
	if !ok {
		return
	}
	if n.Containers == nil {
		n.Containers = map[string]network.EndpointResource{}
	}
	n.Containers[containerID] = network.EndpointResource{Name: containerID}
	f.networks[netName] = n
}

func (f *fakeEngine) detachAllNetworks(containerID string) {
	f.ensureNetworks()
	for name, n := range f.networks {
		if n.Containers != nil {
			delete(n.Containers, containerID)
			f.networks[name] = n
		}
	}
}

func (f *fakeEngine) lookupNetwork(id string) (network.Inspect, bool) {
	f.ensureNetworks()
	if n, ok := f.networks[id]; ok {
		return n, true
	}
	for _, n := range f.networks {
		if n.ID == id || n.Name == id {
			return n, true
		}
	}
	return network.Inspect{}, false
}

func (f *fakeEngine) NetworkCreate(_ context.Context, name string, options network.CreateOptions) (network.CreateResponse, error) {
	f.ops = append(f.ops, "net-create:"+name)
	if f.err != nil {
		return network.CreateResponse{}, f.err
	}
	f.ensureNetworks()
	if _, ok := f.networks[name]; ok {
		return network.CreateResponse{}, errdefs.Conflict(errors.New("network already exists: " + name))
	}
	n := network.Inspect{
		Name:       name,
		ID:         "nid-" + name,
		Driver:     options.Driver,
		Labels:     options.Labels,
		Internal:   options.Internal,
		Containers: map[string]network.EndpointResource{},
	}
	if n.Driver == "" {
		n.Driver = "bridge"
	}
	f.networks[name] = n
	return network.CreateResponse{ID: n.ID}, nil
}

func (f *fakeEngine) NetworkRemove(_ context.Context, networkID string) error {
	f.ops = append(f.ops, "net-remove:"+networkID)
	if f.err != nil {
		return f.err
	}
	n, ok := f.lookupNetwork(networkID)
	if !ok {
		return errdefs.NotFound(errors.New("no such network: " + networkID))
	}
	delete(f.networks, n.Name)
	return nil
}

func (f *fakeEngine) NetworkInspect(_ context.Context, networkID string, _ network.InspectOptions) (network.Inspect, error) {
	f.ops = append(f.ops, "net-inspect:"+networkID)
	if f.err != nil {
		return network.Inspect{}, f.err
	}
	n, ok := f.lookupNetwork(networkID)
	if !ok {
		return network.Inspect{}, errdefs.NotFound(errors.New("no such network: " + networkID))
	}
	return n, nil
}

func (f *fakeEngine) NetworkList(context.Context, network.ListOptions) ([]network.Summary, error) {
	f.ensureNetworks()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]network.Summary, 0, len(f.networks))
	for _, n := range f.networks {
		out = append(out, n)
	}
	return out, nil
}

func (f *fakeEngine) NetworkDisconnect(_ context.Context, networkID, containerID string, _ bool) error {
	f.ops = append(f.ops, "net-disconnect:"+networkID+":"+containerID)
	if f.err != nil {
		return f.err
	}
	n, ok := f.lookupNetwork(networkID)
	if !ok {
		return errdefs.NotFound(errors.New("no such network: " + networkID))
	}
	if n.Containers != nil {
		delete(n.Containers, containerID)
		f.networks[n.Name] = n
	}
	return nil
}

func (f *fakeEngine) ensureVolumes() {
	if f.volumes == nil {
		f.volumes = map[string]volume.Volume{}
	}
}

func (f *fakeEngine) VolumeCreate(_ context.Context, options volume.CreateOptions) (volume.Volume, error) {
	name := options.Name
	f.ops = append(f.ops, "vol-create:"+name)
	if f.err != nil {
		return volume.Volume{}, f.err
	}
	f.ensureVolumes()
	if _, ok := f.volumes[name]; ok {
		return volume.Volume{}, errdefs.Conflict(errors.New("volume already exists: " + name))
	}
	vol := volume.Volume{
		Name:       name,
		Driver:     options.Driver,
		Labels:     options.Labels,
		Mountpoint: "/var/lib/docker/volumes/" + name,
		Scope:      "local",
	}
	if vol.Driver == "" {
		vol.Driver = "local"
	}
	f.volumes[name] = vol
	return vol, nil
}

func (f *fakeEngine) VolumeInspect(_ context.Context, volumeID string) (volume.Volume, error) {
	f.ops = append(f.ops, "vol-inspect:"+volumeID)
	if f.err != nil {
		return volume.Volume{}, f.err
	}
	f.ensureVolumes()
	vol, ok := f.volumes[volumeID]
	if !ok {
		return volume.Volume{}, errdefs.NotFound(errors.New("no such volume: " + volumeID))
	}
	return vol, nil
}

func (f *fakeEngine) VolumeRemove(_ context.Context, volumeID string, _ bool) error {
	f.ops = append(f.ops, "vol-remove:"+volumeID)
	if f.err != nil {
		return f.err
	}
	f.ensureVolumes()
	if _, ok := f.volumes[volumeID]; !ok {
		return errdefs.NotFound(errors.New("no such volume: " + volumeID))
	}
	delete(f.volumes, volumeID)
	return nil
}

func (f *fakeEngine) VolumeList(context.Context, volume.ListOptions) (volume.ListResponse, error) {
	f.ensureVolumes()
	if f.err != nil {
		return volume.ListResponse{}, f.err
	}
	out := make([]*volume.Volume, 0, len(f.volumes))
	for _, vol := range f.volumes {
		v := vol
		out = append(out, &v)
	}
	return volume.ListResponse{Volumes: out}, nil
}

func (f *fakeEngine) ImageBuild(_ context.Context, buildContext io.Reader, options build.ImageBuildOptions) (build.ImageBuildResponse, error) {
	f.ops = append(f.ops, "image-build")
	f.lastBuildTags = append([]string{}, options.Tags...)
	f.lastBuildFiles = map[string]string{}
	if buildContext != nil {
		tr := tar.NewReader(buildContext)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return build.ImageBuildResponse{}, err
			}
			var b strings.Builder
			if _, err := io.Copy(&b, tr); err != nil {
				return build.ImageBuildResponse{}, err
			}
			name := strings.TrimPrefix(hdr.Name, "./")
			f.lastBuildFiles[name] = b.String()
			if name == "Dockerfile" {
				f.lastBuildDockerfile = b.String()
			}
		}
	}
	if f.err != nil {
		return build.ImageBuildResponse{}, f.err
	}
	stream := f.buildStream
	if stream == "" {
		id := f.buildID
		if id == "" {
			id = "sha256:built"
		}
		stream = `{"aux":{"ID":"` + id + `"}}` + "\n"
	}
	return build.ImageBuildResponse{Body: io.NopCloser(strings.NewReader(stream))}, nil
}

func (f *fakeEngine) ContainerExecCreate(_ context.Context, _ string, options container.ExecOptions) (container.ExecCreateResponse, error) {
	f.lastExecCmd = append([]string{}, options.Cmd...)
	if f.execErr != nil {
		return container.ExecCreateResponse{}, f.execErr
	}
	if err := f.containerOpErr(); err != nil {
		return container.ExecCreateResponse{}, err
	}
	return container.ExecCreateResponse{ID: "exec-1"}, nil
}

func (f *fakeEngine) ContainerExecAttach(context.Context, string, container.ExecAttachOptions) (types.HijackedResponse, error) {
	if f.execErr != nil {
		return types.HijackedResponse{}, f.execErr
	}
	if err := f.containerOpErr(); err != nil {
		return types.HijackedResponse{}, err
	}
	a, b := net.Pipe()
	_ = b.Close()
	return types.NewHijackedResponse(a, ""), nil
}

func (f *fakeEngine) ContainerExecInspect(_ context.Context, execID string) (container.ExecInspect, error) {
	if f.execErr != nil {
		return container.ExecInspect{}, f.execErr
	}
	if err := f.containerOpErr(); err != nil {
		return container.ExecInspect{}, err
	}
	return container.ExecInspect{ExecID: execID, ExitCode: f.execExit}, nil
}

func (f *fakeEngine) Close() error       { return nil }
func (f *fakeEngine) DaemonHost() string { return f.host }

func TestClient_PingMapsBasicInfo(t *testing.T) {
	c := dockerx.NewWithAPI(&fakeEngine{
		host: "tcp://127.0.0.1:2375",
		ping: types.Ping{APIVersion: "1.45", OSType: "linux", Experimental: true},
	})
	got, err := c.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got.APIVersion != "1.45" || got.OSType != "linux" || !got.Experimental {
		t.Fatalf("Ping = %+v", got)
	}
	if got.Host != "tcp://127.0.0.1:2375" {
		t.Fatalf("Host = %q", got.Host)
	}
}

func TestClient_VersionMapsFields(t *testing.T) {
	c := dockerx.NewWithAPI(&fakeEngine{
		ver: types.Version{
			Version:       "27.0.0",
			APIVersion:    "1.45",
			MinAPIVersion: "1.24",
			GitCommit:     "abc",
			GoVersion:     "go1.22",
			Os:            "linux",
			Arch:          "amd64",
			BuildTime:     "2026-01-01",
		},
	})
	got, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got.Version != "27.0.0" || got.APIVersion != "1.45" || got.Os != "linux" {
		t.Fatalf("Version = %+v", got)
	}
}

func TestClient_InfoMapsEngineSummary(t *testing.T) {
	c := dockerx.NewWithAPI(&fakeEngine{
		info: system.Info{
			ID:                "engine-1",
			Name:              "dev-host",
			ServerVersion:     "27.0.0",
			OperatingSystem:   "Ubuntu",
			OSType:            "linux",
			Architecture:      "x86_64",
			NCPU:              8,
			MemTotal:          16 << 30,
			Driver:            "overlay2",
			DockerRootDir:     "/var/lib/docker",
			Containers:        4,
			ContainersRunning: 1,
			ContainersPaused:  0,
			ContainersStopped: 3,
			Images:            12,
			KernelVersion:     "6.8.0",
		},
	})
	got, err := c.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if got.ID != "engine-1" || got.ContainersRunning != 1 || got.Images != 12 || got.NCPU != 8 {
		t.Fatalf("Info = %+v", got)
	}
}

func TestClient_PingWrapsNotFound(t *testing.T) {
	c := dockerx.NewWithAPI(&fakeEngine{err: errdefs.NotFound(errors.New("missing"))})
	_, err := c.Ping(context.Background())
	if !sandboxerr.IsNotFound(err) {
		t.Fatalf("Ping err = %v, want not_found", err)
	}
}

func TestClient_PingWrapsUnavailable(t *testing.T) {
	c := dockerx.NewWithAPI(&fakeEngine{
		err: errors.New("Cannot connect to the Docker daemon at npipe:////./pipe/docker_engine. Is the docker daemon running?"),
	})
	_, err := c.Ping(context.Background())
	if !sandboxerr.IsUnavailable(err) {
		t.Fatalf("Ping err = %v, want unavailable", err)
	}
}

func TestClient_ListContainersMapsAll(t *testing.T) {
	c := dockerx.NewWithAPI(&fakeEngine{
		containers: []container.Summary{
			{
				ID:      "abc123def456",
				Names:   []string{"/sandbox-1"},
				Image:   "golang:1.22",
				ImageID: "sha256:111",
				Command: "go test",
				Created: 1700000000,
				State:   container.StateRunning,
				Status:  "Up 2 minutes",
				Ports:   []container.Port{{IP: "0.0.0.0", PrivatePort: 8080, PublicPort: 18080, Type: "tcp"}},
			},
			{
				ID:     "deadbeef0001",
				Names:  []string{"/exited-job"},
				Image:  "alpine:3.20",
				State:  container.StateExited,
				Status: "Exited (0) 1 hour ago",
			},
		},
	})
	got, err := c.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "abc123def456" || got[0].Image != "golang:1.22" || got[0].State != "running" {
		t.Fatalf("container[0] = %+v", got[0])
	}
	if got[0].Ports[0].PublicPort != 18080 {
		t.Fatalf("port = %+v", got[0].Ports)
	}
	if got[1].State != "exited" || got[1].Names[0] != "/exited-job" {
		t.Fatalf("container[1] = %+v", got[1])
	}
}

func TestClient_ListContainersEmpty(t *testing.T) {
	c := dockerx.NewWithAPI(&fakeEngine{})
	got, err := c.ListContainers(context.Background())
	if err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want empty slice", got)
	}
}

func TestClient_ListImagesMapsAll(t *testing.T) {
	c := dockerx.NewWithAPI(&fakeEngine{
		images: []image.Summary{
			{
				ID:          "sha256:aaa",
				ParentID:    "sha256:bbb",
				RepoTags:    []string{"golang:1.22", "golang:latest"},
				RepoDigests: []string{"golang@sha256:aaa"},
				Created:     1700000000,
				Size:        800 << 20,
				SharedSize:  100 << 20,
				Containers:  2,
			},
			{
				ID:       "sha256:untagged",
				RepoTags: nil,
				Size:     12 << 20,
			},
		},
	})
	got, err := c.ListImages(context.Background())
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "sha256:aaa" || len(got[0].RepoTags) != 2 || got[0].Size != 800<<20 {
		t.Fatalf("image[0] = %+v", got[0])
	}
	if got[1].ID != "sha256:untagged" {
		t.Fatalf("image[1] = %+v", got[1])
	}
}

func TestClient_ListImagesWrapsUnavailable(t *testing.T) {
	c := dockerx.NewWithAPI(&fakeEngine{
		err: errors.New("Cannot connect to the Docker daemon. Is the docker daemon running?"),
	})
	_, err := c.ListImages(context.Background())
	if !sandboxerr.IsUnavailable(err) {
		t.Fatalf("ListImages err = %v, want unavailable", err)
	}
}
