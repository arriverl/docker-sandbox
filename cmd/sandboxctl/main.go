package main

import (
	"context"
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
	maxInstances := flag.Int("max-instances", sandbox.DesignPeakInstances, "max active instances; 0 disables the cap")
	jsonOut := flag.Bool("json", false, "emit JSON instead of human-readable text")
	journal := flag.String("journal", "", "compensation journal path; empty keeps pending in memory only")
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
	observer := cliObserver{jsonMode: *jsonOut, human: os.Stderr, inner: sandbox.LogObserver{}}
	sb := dockerx.NewUserSandbox(client, dockerx.UserSandboxConfig{
		Prefix:          *prefix,
		DefaultMemory:   defaultMemoryBytes,
		DefaultNanoCPUs: defaultNanoCPUs,
		MaxInstances:    *maxInstances,
		Observer:        observer,
		JournalPath:     *journal,
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
		items, listErr := orch.List(ctx)
		err = listErr
		out = psResult{Instances: items, PortPool: sb.PortPoolStats()}
	case "containers":
		out, err = mgr.List(ctx)
	case "images":
		out, err = client.ListImages(ctx)
	case "create":
		out, err = runCreate(ctx, mgr, sb, orch, flag.Args()[1:])
	case "legacy-create":
		out, err = runLegacyCreate(ctx, mgr, sb, flag.Args()[1:])
	case "ensure":
		out, err = runEnsure(ctx, orch, flag.Args()[1:])
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
	if err := writeOutput(*jsonOut, out); err != nil {
		fatal(err)
	}
}

func runCreate(ctx context.Context, mgr *dockerx.ContainerManager, sb *dockerx.UserSandbox, orch *sandbox.Service, args []string) (any, error) {
	spec, _, err := parseInstanceSpec("create", args)
	if err != nil {
		return nil, err
	}
	if spec.InstanceID == "" {
		return nil, rejectCreateWithoutInstance()
	}
	return orch.Create(ctx, spec)
}

func runLegacyCreate(ctx context.Context, mgr *dockerx.ContainerManager, sb *dockerx.UserSandbox, args []string) (any, error) {
	return runRawCreate(ctx, mgr, sb, args)
}

func runEnsure(ctx context.Context, orch *sandbox.Service, args []string) (any, error) {
	spec, probe, err := parseInstanceSpec("ensure", args)
	if err != nil {
		return nil, err
	}
	if spec.InstanceID == "" {
		return nil, rejectCreateWithoutInstance()
	}
	return orch.EnsureRunning(ctx, spec, probe)
}

func parseInstanceSpec(name string, args []string) (sandbox.Spec, sandbox.Probe, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	image := fs.String("image", "", "image name (required)")
	cmd := fs.String("cmd", "", "command, space-separated")
	expose := fs.String("expose", "", "container ports allocated from host port pool, e.g. 80,443/tcp")
	userID := fs.String("user", "", "user id; required for instance orchestration")
	instance := fs.String("instance", "", "stable instance id (platform primary key)")
	memory := fs.String("memory", "", "memory limit, e.g. 64m")
	cpus := fs.String("cpus", "", "CPU limit, e.g. 0.5")
	skipPull := fs.Bool("skip-pull", false, "do not pull missing images")
	netInstance := fs.Bool("net-instance", false, "give this instance its own Docker network")
	httpPath := fs.String("http", "", "HTTP path on first host port, e.g. /ready")
	tcp := fs.Bool("tcp", false, "TCP connect to first host port")
	execCmd := fs.String("exec", "", "command to exec inside the container")
	port := fs.Int("port", 0, "host port override")
	if err := fs.Parse(args); err != nil {
		return sandbox.Spec{}, sandbox.Probe{}, err
	}
	spec := sandbox.Spec{
		InstanceID: *instance,
		UserID:     *userID,
		Image:      *image,
		Expose:     splitCSV(*expose),
		SkipPull:   *skipPull,
	}
	if *netInstance {
		spec.NetworkIsolation = sandbox.NetworkPerInstance
	}
	if *cmd != "" {
		spec.Cmd = strings.Fields(*cmd)
	}
	if *memory != "" {
		n, err := parseMemory(*memory)
		if err != nil {
			return sandbox.Spec{}, sandbox.Probe{}, err
		}
		spec.Memory = n
	}
	if *cpus != "" {
		n, err := parseCPUs(*cpus)
		if err != nil {
			return sandbox.Spec{}, sandbox.Probe{}, err
		}
		spec.NanoCPUs = n
	}
	probe := sandbox.Probe{Port: *port}
	switch {
	case strings.TrimSpace(*execCmd) != "":
		probe.Kind = sandbox.ProbeExec
		probe.Command = strings.Fields(*execCmd)
	case *tcp:
		probe.Kind = sandbox.ProbeTCP
	case strings.TrimSpace(*httpPath) != "":
		probe.Kind = sandbox.ProbeHTTP
		probe.Path = *httpPath
	}
	return spec, probe, nil
}

func runRawCreate(ctx context.Context, mgr *dockerx.ContainerManager, sb *dockerx.UserSandbox, args []string) (any, error) {
	fs := flag.NewFlagSet("legacy-create", flag.ContinueOnError)
	image := fs.String("image", "", "image name (required)")
	name := fs.String("name", "", "container name")
	cmd := fs.String("cmd", "", "command, space-separated")
	publish := fs.String("publish", "", "comma-separated ports, e.g. 8080:80/tcp")
	expose := fs.String("expose", "", "container ports allocated from host port pool, e.g. 80,443/tcp")
	userID := fs.String("user", "", "user id")
	memory := fs.String("memory", "", "memory limit, e.g. 64m")
	cpus := fs.String("cpus", "", "CPU limit, e.g. 0.5")
	autoRemove := fs.Bool("rm", false, "auto-remove when container exits")
	skipPull := fs.Bool("skip-pull", false, "do not pull missing images")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	opts := dockerx.CreateOptions{
		Image:      *image,
		Name:       *name,
		AutoRemove: *autoRemove,
		SkipPull:   *skipPull,
		UserID:     *userID,
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
			return nil, fmt.Errorf("legacy-create -expose requires -user")
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
	fmt.Fprint(flag.CommandLine.Output(), usageText())
	flag.PrintDefaults()
}

func fatal(err error) {
	kind := sandboxerr.Classify(err)
	action := sandboxerr.ActionOf(err)
	if action != "" {
		fmt.Fprintf(os.Stderr, "sandboxctl: [%s] next=%s %v\n", kind, action, err)
	} else {
		fmt.Fprintf(os.Stderr, "sandboxctl: [%s] %v\n", kind, err)
	}
	os.Exit(1)
}
