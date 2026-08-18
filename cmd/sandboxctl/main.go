package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gitproduct/docker-sandbox/dockerx"
	"github.com/gitproduct/docker-sandbox/sandbox"
	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

func main() {
	host := flag.String("host", "", "Docker host (unix://, npipe:////, tcp://)")
	proxy := flag.String("proxy", "", "HTTP proxy URL, only for tcp/http/https hosts")
	noProxy := flag.Bool("no-proxy", false, "disable HTTP proxy (ignore HTTP_PROXY/HTTPS_PROXY)")
	timeout := flag.Duration("timeout", 5*time.Minute, "request timeout (create may pull images)")
	prefix := flag.String("prefix", dockerx.DefaultNetworkPrefix, "name prefix for {prefix}-net-{userId} and {prefix}-ws-{instanceId}")
	defaultMemory := flag.String("default-memory", "256m", "default instance memory limit")
	defaultCPUs := flag.String("default-cpus", "0.5", "default instance CPU limit")
	flag.Usage = usage
	flag.Parse()

	cmd := flag.Arg(0)
	if cmd == "" {
		cmd = "ping"
	}

	client, err := dockerx.New(dockerx.Config{
		Host:         *host,
		ProxyURL:     *proxy,
		DisableProxy: *noProxy,
		Timeout:      *timeout,
	})
	if err != nil {
		fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	defaultMemoryBytes, err := parseMemory(*defaultMemory)
	if err != nil {
		fatal(err)
	}
	defaultNanoCPUs, err := parseCPUs(*defaultCPUs)
	if err != nil {
		fatal(err)
	}

	mgr := client.Containers()
	sb := dockerx.NewUserSandbox(client, dockerx.UserSandboxConfig{
		Prefix:          *prefix,
		DefaultMemory:   defaultMemoryBytes,
		DefaultNanoCPUs: defaultNanoCPUs,
	})
	orch, err := dockerx.BindOrchestrator(client, sb, *prefix)
	if err != nil {
		fatal(err)
	}

	var out any
	switch cmd {
	case "ping":
		out, err = client.Ping(ctx)
	case "version":
		out, err = client.Version(ctx)
	case "info":
		out, err = client.Info(ctx)
	case "ps":
		out, err = orch.List(ctx)
	case "containers":
		out, err = mgr.List(ctx)
	case "images":
		out, err = client.ListImages(ctx)
	case "create":
		out, err = runCreate(ctx, mgr, sb, orch, flag.Args()[1:])
	case "start":
		out, err = runStart(ctx, orch, flag.Args()[1:])
	case "stop":
		out, err = runStop(ctx, orch, flag.Args()[1:], false)
	case "restart":
		out, err = runStop(ctx, orch, flag.Args()[1:], true)
	case "remove", "rm":
		out, err = runRemove(ctx, sb, flag.Args()[1:])
	case "inspect":
		out, err = runInspect(ctx, orch, flag.Args()[1:])
	case "active":
		uid := requireArg("active", flag.Args()[1:])
		sb.MarkActive(uid)
		out = map[string]string{"status": "active", "user": uid, "network": dockerx.UserNetworkName(*prefix, uid)}
	case "inactive":
		uid := requireArg("inactive", flag.Args()[1:])
		sb.MarkInactive(uid)
		out = map[string]string{"status": "inactive", "user": uid}
	case "reap":
		if len(flag.Args()) > 1 && strings.TrimSpace(flag.Arg(1)) != "" {
			uid := flag.Arg(1)
			err = sb.ReapUser(ctx, uid)
			out = map[string]string{"status": "reaped", "user": uid, "network": dockerx.UserNetworkName(*prefix, uid)}
		} else {
			err = sb.Reap(ctx)
			out = map[string]string{"status": "reaped"}
		}
	case "destroy":
		err = runDestroy(ctx, orch, flag.Args()[1:])
		out = map[string]string{"status": "destroyed"}
	case "restore":
		out, err = runRestore(ctx, orch, flag.Args()[1:])
	case "wait":
		out, err = runWait(ctx, orch, flag.Args()[1:])
	case "volumes":
		out, err = sb.Workspace().List(ctx)
	case "volume":
		id := requireArg("volume", flag.Args()[1:])
		out, err = sb.Workspace().Inspect(ctx, id)
	case "build":
		out, err = runBuild(ctx, client.Images(), flag.Args()[1:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fatal(err)
	}
}

func runCreate(ctx context.Context, mgr *dockerx.ContainerManager, sb *dockerx.UserSandbox, orch *sandbox.Service, args []string) (any, error) {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	image := fs.String("image", "", "image name (required)")
	name := fs.String("name", "", "container name (legacy path only)")
	cmd := fs.String("cmd", "", "command, space-separated")
	publish := fs.String("publish", "", "comma-separated ports, e.g. 8080:80/tcp")
	expose := fs.String("expose", "", "container ports allocated from host port pool, e.g. 80,443/tcp")
	userID := fs.String("user", "", "user id; required for instance orchestration")
	instance := fs.String("instance", "", "stable instance id (platform primary key)")
	memory := fs.String("memory", "", "memory limit, e.g. 64m")
	cpus := fs.String("cpus", "", "CPU limit, e.g. 0.5")
	autoRemove := fs.Bool("rm", false, "auto-remove when container exits (legacy path only)")
	skipPull := fs.Bool("skip-pull", false, "do not pull missing images")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(*instance) != "" {
		spec := sandbox.Spec{
			InstanceID: *instance,
			UserID:     *userID,
			Image:      *image,
			Expose:     splitCSV(*expose),
			SkipPull:   *skipPull,
		}
		if *cmd != "" {
			spec.Cmd = strings.Fields(*cmd)
		}
		if *memory != "" {
			n, err := parseMemory(*memory)
			if err != nil {
				return nil, err
			}
			spec.Memory = n
		}
		if *cpus != "" {
			n, err := parseCPUs(*cpus)
			if err != nil {
				return nil, err
			}
			spec.NanoCPUs = n
		}
		return orch.Create(ctx, spec)
	}
	opts := dockerx.CreateOptions{
		Image:      *image,
		Name:       *name,
		AutoRemove: *autoRemove,
		SkipPull:   *skipPull,
		UserID:     *userID,
		InstanceID: *instance,
	}
	if *cmd != "" {
		opts.Cmd = strings.Fields(*cmd)
	}
	if *publish != "" {
		opts.Ports = splitCSV(*publish)
	}
	if *expose != "" {
		opts.Expose = splitCSV(*expose)
	}
	if *memory != "" {
		n, err := parseMemory(*memory)
		if err != nil {
			return nil, err
		}
		opts.Memory = n
	}
	if *cpus != "" {
		n, err := parseCPUs(*cpus)
		if err != nil {
			return nil, err
		}
		opts.NanoCPUs = n
	}
	if strings.TrimSpace(opts.UserID) != "" || len(opts.Expose) > 0 {
		if strings.TrimSpace(opts.UserID) == "" {
			return nil, fmt.Errorf("create -expose requires -user")
		}
		return sb.Create(ctx, opts)
	}
	return mgr.Create(ctx, opts)
}

func runStart(ctx context.Context, orch *sandbox.Service, args []string) (any, error) {
	id := requireArg("start", args)
	return orch.Start(ctx, id)
}

func runInspect(ctx context.Context, orch *sandbox.Service, args []string) (any, error) {
	id := requireArg("inspect", args)
	return orch.Get(ctx, id)
}

func runStop(ctx context.Context, orch *sandbox.Service, args []string, restart bool) (any, error) {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	wait := fs.Int("t", -1, "seconds to wait before SIGKILL; negative uses engine default")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	id := requireArg("stop", fs.Args())
	opts := sandbox.StopOptions{}
	if *wait >= 0 {
		opts.Timeout = wait
	}
	if restart {
		return orch.Restart(ctx, id, opts)
	}
	return orch.Stop(ctx, id, opts)
}

func runRemove(ctx context.Context, sb *dockerx.UserSandbox, args []string) (map[string]any, error) {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	force := fs.Bool("force", false, "remove a running container")
	volumes := fs.Bool("volumes", false, "remove associated anonymous volumes")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	id := requireArg("remove", fs.Args())
	if err := sb.Remove(ctx, id, dockerx.RemoveOptions{Force: *force, RemoveVolumes: *volumes}); err != nil {
		return nil, err
	}
	return map[string]any{"status": "removed", "id": id}, nil
}

func runDestroy(ctx context.Context, orch *sandbox.Service, args []string) error {
	fs := flag.NewFlagSet("destroy", flag.ContinueOnError)
	instance := fs.String("instance", "", "instance id")
	purge := fs.Bool("purge", false, "also delete workspace volume")
	rmVolume := fs.Bool("rm-volume", false, "alias of -purge")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id := strings.TrimSpace(*instance)
	if id == "" {
		id = requireArg("destroy", fs.Args())
	}
	return orch.Destroy(ctx, id, sandbox.DestroyOptions{Purge: *purge || *rmVolume})
}

func runRestore(ctx context.Context, orch *sandbox.Service, args []string) (sandbox.Instance, error) {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	instance := fs.String("instance", "", "instance id")
	reset := fs.Bool("reset", false, "discard existing volume and create an empty one")
	if err := fs.Parse(args); err != nil {
		return sandbox.Instance{}, err
	}
	id := strings.TrimSpace(*instance)
	if id == "" {
		id = requireArg("restore", fs.Args())
	}
	return orch.Restore(ctx, id, sandbox.RestoreOptions{KeepVolume: !*reset, ResetVolume: *reset})
}

func runWait(ctx context.Context, orch *sandbox.Service, args []string) (sandbox.Instance, error) {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	httpPath := fs.String("http", "", "HTTP path on first host port, e.g. /ready")
	tcp := fs.Bool("tcp", false, "TCP connect to first host port")
	execCmd := fs.String("exec", "", "command to exec inside the container")
	port := fs.Int("port", 0, "host port override")
	if err := fs.Parse(args); err != nil {
		return sandbox.Instance{}, err
	}
	id := requireArg("wait", fs.Args())
	probe := sandbox.Probe{Port: *port}
	switch {
	case strings.TrimSpace(*execCmd) != "":
		probe.Kind = sandbox.ProbeExec
		probe.Command = strings.Fields(*execCmd)
	case *tcp:
		probe.Kind = sandbox.ProbeTCP
	default:
		probe.Kind = sandbox.ProbeHTTP
		probe.Path = *httpPath
	}
	return orch.WaitReady(ctx, id, probe)
}

func runBuild(ctx context.Context, b *dockerx.ImageBuilder, args []string) (dockerx.BuildResult, error) {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	tag := fs.String("tag", "", "image tag (required), e.g. sandbox/demo:1")
	base := fs.String("base", "", "base image for generated Dockerfile")
	dockerfile := fs.String("dockerfile", "", "path to a custom Dockerfile")
	env := fs.String("env", "", "comma-separated KEY=VAL")
	label := fs.String("label", "", "comma-separated KEY=VAL image labels")
	inject := fs.String("inject", "", "comma-separated host:dest files to COPY")
	step := fs.String("step", "", "extra Dockerfile instruction; repeat via commas")
	workdir := fs.String("workdir", "", "WORKDIR")
	user := fs.String("user", "", "USER")
	cmd := fs.String("cmd", "", "CMD, space-separated")
	noCache := fs.Bool("no-cache", false, "disable build cache")
	pull := fs.Bool("pull", false, "always pull base image")
	if err := fs.Parse(args); err != nil {
		return dockerx.BuildResult{}, err
	}
	spec := dockerx.BuildSpec{
		Tag:       *tag,
		BaseImage: *base,
		Workdir:   *workdir,
		User:      *user,
		NoCache:   *noCache,
		Pull:      *pull,
		Env:       parseKV(*env),
		Labels:    parseKV(*label),
	}
	if *dockerfile != "" {
		raw, err := os.ReadFile(*dockerfile)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return dockerx.BuildResult{}, sandboxerr.WrapKind("读取 Dockerfile", sandboxerr.KindNotFound,
					fmt.Errorf("%w: %v", sandboxerr.ErrNotFound, err))
			}
			return dockerx.BuildResult{}, err
		}
		spec.Dockerfile = string(raw)
	}
	if *cmd != "" {
		spec.Cmd = strings.Fields(*cmd)
	}
	if *step != "" {
		spec.Steps = splitDockerfileSteps(*step)
	}
	if *inject != "" {
		files, err := parseInject(*inject)
		if err != nil {
			return dockerx.BuildResult{}, err
		}
		spec.Inject = files
	}
	return b.Build(ctx, spec)
}

func parseKV(s string) map[string]string {
	out := map[string]string{}
	for _, item := range splitCSV(s) {
		k, v, ok := strings.Cut(item, "=")
		if !ok || strings.TrimSpace(k) == "" {
			continue
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

func splitDockerfileSteps(s string) []string {
	parts := strings.Split(s, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseInject(s string) ([]dockerx.InjectFile, error) {
	var out []dockerx.InjectFile
	for _, item := range splitCSV(s) {
		src, dest, err := dockerx.SplitInjectSpec(item)
		if err != nil {
			return nil, err
		}
		out = append(out, dockerx.InjectFile{Path: dest, Source: src})
	}
	return out, nil
}

func requireArg(cmd string, args []string) string {
	if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
		fatal(fmt.Errorf("%s requires an argument", cmd))
	}
	return args[0]
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseMemory(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, strings.TrimSuffix(s, "g")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, sandboxerr.WrapKind("解析内存限制", sandboxerr.KindInvalid,
			fmt.Errorf("%w: invalid memory limit %q", sandboxerr.ErrInvalid, s))
	}
	return n * mult, nil
}

func parseCPUs(s string) (int64, error) {
	s = strings.TrimSpace(s)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, sandboxerr.WrapKind("解析 CPU 限制", sandboxerr.KindInvalid,
			fmt.Errorf("%w: invalid cpu limit %q", sandboxerr.ErrInvalid, s))
	}
	return int64(f * 1e9), nil
}

func usage() {
	fmt.Fprintf(flag.CommandLine.Output(), `usage: sandboxctl [global flags] <command> [command flags]

instance commands (platform primary key is instance_id):
  create   -instance ID -user USER -image IMAGE [-memory 64m] [-cpus 0.5] [-expose 80] [-cmd "..."]
  start    INSTANCE
  stop     [-t SECONDS] INSTANCE
  restart  [-t SECONDS] INSTANCE
  destroy  INSTANCE [-purge]
  restore  INSTANCE [-reset]
  inspect  INSTANCE
  ps
  wait     INSTANCE [-http PATH] [-tcp] [-exec CMD] [-port N]

legacy container commands:
  containers | images | ping | version | info
  create   -image IMAGE [-name NAME] [-user USER] [-cmd "..."] [-publish 8080:80/tcp] [-memory 64m] [-rm] [-skip-pull]
  remove   [-force] [-volumes] CONTAINER_ID
  active USER | inactive USER | reap [USER]
  volume INSTANCE | volumes
  build    -tag NAME -base IMAGE [-dockerfile PATH] [-env K=V] [-label K=V] [-inject host:dest] [-step "RUN ..."] [-workdir DIR] [-cmd "..."] [-no-cache] [-pull]

`)
	flag.PrintDefaults()
}

func fatal(err error) {
	kind := sandboxerr.Classify(err)
	fmt.Fprintf(os.Stderr, "sandboxctl: [%s] %v\n", kind, err)
	os.Exit(1)
}
