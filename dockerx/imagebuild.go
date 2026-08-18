package dockerx

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/build"

	"github.com/gitproduct/docker-sandbox/sandboxerr"
)

// InjectFile 是构建时写入镜像上下文并 COPY 进镜像的文件。
type InjectFile struct {
	Path    string // 镜像内目标路径，如 /app/main.py
	Content []byte
	Source  string // 宿主机文件路径；Content 为空时读取
}

// BuildSpec 描述一次动态镜像构建。
type BuildSpec struct {
	Tag        string
	BaseImage  string
	Dockerfile string // 自定义 Dockerfile；为空则根据 BaseImage/Steps 等生成
	Steps      []string
	Inject     []InjectFile
	Env        map[string]string
	Labels     map[string]string
	Workdir    string
	User       string
	Cmd        []string
	Entrypoint []string
	BuildArgs  map[string]string
	NoCache    bool
	Pull       bool
	Target     string
}

// BuildResult 是镜像构建结果。
type BuildResult struct {
	ID         string
	Tag        string
	Dockerfile string
	Logs       string
}

// ImageBuilder 根据规格打包上下文并调用 Docker 构建。
type ImageBuilder struct {
	client *Client
}

// NewImageBuilder 构造镜像构建器。
func NewImageBuilder(c *Client) *ImageBuilder {
	return &ImageBuilder{client: c}
}

// Images 返回绑定到当前客户端的镜像构建器。
func (c *Client) Images() *ImageBuilder {
	return NewImageBuilder(c)
}

func (b *ImageBuilder) ready() error {
	if b == nil || b.client == nil {
		return sandboxerr.WrapKind("构建镜像", sandboxerr.KindInvalid,
			fmt.Errorf("构建镜像失败：容器管理器未初始化"))
	}
	return nil
}

// RenderDockerfile 根据规格生成 Dockerfile 文本，不触发构建。
func RenderDockerfile(spec BuildSpec) (string, error) {
	files, err := injectContextFiles(spec.Inject)
	if err != nil {
		return "", err
	}
	return renderDockerfile(spec, files)
}

func renderDockerfile(spec BuildSpec, files []injectEntry) (string, error) {
	var b strings.Builder
	custom := strings.TrimSpace(spec.Dockerfile)
	base := strings.TrimSpace(spec.BaseImage)
	if custom != "" {
		if base != "" && !hasFromInstruction(custom) {
			b.WriteString("FROM ")
			b.WriteString(base)
			b.WriteByte('\n')
		}
		b.WriteString(custom)
		if !strings.HasSuffix(custom, "\n") {
			b.WriteByte('\n')
		}
		writeCopyLines(&b, files)
		return b.String(), nil
	}
	if base == "" {
		return "", sandboxerr.WrapKind("image.build", sandboxerr.KindInvalid,
			fmt.Errorf("%w: base image is required", sandboxerr.ErrInvalid))
	}
	b.WriteString("FROM ")
	b.WriteString(base)
	b.WriteByte('\n')
	for _, k := range sortedKeys(spec.BuildArgs) {
		b.WriteString("ARG ")
		b.WriteString(k)
		b.WriteByte('\n')
	}
	for _, k := range sortedKeys(spec.Env) {
		b.WriteString("ENV ")
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strconv.Quote(spec.Env[k]))
		b.WriteByte('\n')
	}
	for _, k := range sortedKeys(spec.Labels) {
		b.WriteString("LABEL ")
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strconv.Quote(spec.Labels[k]))
		b.WriteByte('\n')
	}
	writeCopyLines(&b, files)
	for _, step := range spec.Steps {
		step = strings.TrimSpace(step)
		if step == "" {
			continue
		}
		b.WriteString(step)
		b.WriteByte('\n')
	}
	if wd := strings.TrimSpace(spec.Workdir); wd != "" {
		b.WriteString("WORKDIR ")
		b.WriteString(wd)
		b.WriteByte('\n')
	}
	if user := strings.TrimSpace(spec.User); user != "" {
		b.WriteString("USER ")
		b.WriteString(user)
		b.WriteByte('\n')
	}
	if len(spec.Entrypoint) > 0 {
		b.WriteString("ENTRYPOINT ")
		b.WriteString(jsonStringArray(spec.Entrypoint))
		b.WriteByte('\n')
	}
	if len(spec.Cmd) > 0 {
		b.WriteString("CMD ")
		b.WriteString(jsonStringArray(spec.Cmd))
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func hasFromInstruction(dockerfile string) bool {
	for _, line := range strings.Split(dockerfile, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return strings.HasPrefix(strings.ToUpper(line), "FROM ")
	}
	return false
}

func writeCopyLines(b *strings.Builder, files []injectEntry) {
	for _, f := range files {
		b.WriteString("COPY ")
		b.WriteString(f.contextPath)
		b.WriteByte(' ')
		b.WriteString(f.dest)
		b.WriteByte('\n')
	}
}

func jsonStringArray(items []string) string {
	buf, err := json.Marshal(items)
	if err != nil {
		return "[]"
	}
	return string(buf)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type injectEntry struct {
	contextPath string
	dest        string
	content     []byte
}

func injectContextFiles(inject []InjectFile) ([]injectEntry, error) {
	seen := map[string]bool{}
	out := make([]injectEntry, 0, len(inject))
	for _, item := range inject {
		dest := strings.TrimSpace(item.Path)
		ctxPath, err := contextPath(dest)
		if err != nil {
			return nil, err
		}
		if seen[ctxPath] {
			return nil, sandboxerr.WrapKind("image.build", sandboxerr.KindInvalid,
				fmt.Errorf("%w: duplicate inject path %q", sandboxerr.ErrInvalid, dest))
		}
		seen[ctxPath] = true
		content := item.Content
		if len(content) == 0 && strings.TrimSpace(item.Source) != "" {
			raw, err := os.ReadFile(item.Source)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, sandboxerr.WrapKind("image.build", sandboxerr.KindNotFound,
						fmt.Errorf("%w: inject file not found: %s", sandboxerr.ErrNotFound, item.Source))
				}
				return nil, sandboxerr.WrapKind("image.build", sandboxerr.KindInvalid,
					fmt.Errorf("%w: read inject file %s: %v", sandboxerr.ErrInvalid, item.Source, err))
			}
			content = raw
		}
		absDest := dest
		if !strings.HasPrefix(absDest, "/") {
			absDest = "/" + absDest
		}
		out = append(out, injectEntry{contextPath: ctxPath, dest: absDest, content: content})
	}
	return out, nil
}

// SplitInjectSpec 解析 host:dest。dest 以 / 开头时按最后一个 ":/" 分割，避免 Windows 盘符被截断。
func SplitInjectSpec(item string) (src, dest string, err error) {
	item = strings.TrimSpace(item)
	if item == "" {
		return "", "", fmt.Errorf("invalid inject %q, want host:dest", item)
	}
	if i := strings.LastIndex(item, ":/"); i >= 0 {
		src, dest = strings.TrimSpace(item[:i]), strings.TrimSpace(item[i+1:])
		if src == "" || dest == "" {
			return "", "", fmt.Errorf("invalid inject %q, want host:dest", item)
		}
		return src, dest, nil
	}
	src, dest, ok := strings.Cut(item, ":")
	src, dest = strings.TrimSpace(src), strings.TrimSpace(dest)
	if !ok || src == "" || dest == "" {
		return "", "", fmt.Errorf("invalid inject %q, want host:dest", item)
	}
	return src, dest, nil
}

func contextPath(dest string) (string, error) {
	dest = strings.ReplaceAll(strings.TrimSpace(dest), "\\", "/")
	if dest == "" || dest == "/" {
		return "", sandboxerr.WrapKind("image.build", sandboxerr.KindInvalid,
			fmt.Errorf("%w: invalid inject path %q", sandboxerr.ErrInvalid, dest))
	}
	dest = strings.TrimPrefix(dest, "/")
	clean := path.Clean(dest)
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".." || seg == "." && clean == "." {
			return "", sandboxerr.WrapKind("image.build", sandboxerr.KindInvalid,
				fmt.Errorf("%w: invalid inject path", sandboxerr.ErrInvalid))
		}
	}
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", sandboxerr.WrapKind("image.build", sandboxerr.KindInvalid,
			fmt.Errorf("%w: invalid inject path", sandboxerr.ErrInvalid))
	}
	return clean, nil
}

func packBuildContext(dockerfile string, files []injectEntry) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := writeTarFile(tw, "Dockerfile", []byte(dockerfile)); err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := writeTarFile(tw, f.contextPath, f.content); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeTarFile(tw *tar.Writer, name string, body []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	_, err := tw.Write(body)
	return err
}

type buildStreamMsg struct {
	Stream      string `json:"stream"`
	Error       string `json:"error"`
	ErrorDetail struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
	Aux *struct {
		ID string `json:"ID"`
	} `json:"aux"`
}

func parseBuildStream(r io.Reader) (id, logs string, err error) {
	dec := json.NewDecoder(r)
	var log strings.Builder
	for {
		var msg buildStreamMsg
		if derr := dec.Decode(&msg); derr != nil {
			if derr == io.EOF {
				break
			}
			return "", log.String(), sandboxerr.WrapKind("image.build", sandboxerr.KindInvalid,
				fmt.Errorf("build failed: invalid build stream: %v", derr))
		}
		if msg.Stream != "" {
			log.WriteString(msg.Stream)
		}
		if msg.Aux != nil && msg.Aux.ID != "" {
			id = msg.Aux.ID
		}
		fail := strings.TrimSpace(msg.Error)
		if fail == "" {
			fail = strings.TrimSpace(msg.ErrorDetail.Message)
		}
		if fail != "" {
			return "", log.String(), sandboxerr.WrapKind("image.build", sandboxerr.KindInvalid,
				fmt.Errorf("build failed: %s", fail))
		}
	}
	return id, log.String(), nil
}

func ptrStringMap(in map[string]string) map[string]*string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*string, len(in))
	for k, v := range in {
		v := v
		out[k] = &v
	}
	return out
}

// Build 根据规格动态构建镜像。
func (b *ImageBuilder) Build(ctx context.Context, spec BuildSpec) (BuildResult, error) {
	if err := b.ready(); err != nil {
		return BuildResult{}, err
	}
	tag := strings.TrimSpace(spec.Tag)
	if tag == "" {
		return BuildResult{}, managerError("构建镜像",
			sandboxerr.WrapKind("image.build", sandboxerr.KindInvalid,
				fmt.Errorf("%w: image tag is empty", sandboxerr.ErrInvalid)),
			errorContext{})
	}
	files, err := injectContextFiles(spec.Inject)
	if err != nil {
		return BuildResult{}, managerError("构建镜像", err, errorContext{Image: tag})
	}
	df, err := renderDockerfile(spec, files)
	if err != nil {
		return BuildResult{}, managerError("构建镜像", err, errorContext{Image: tag})
	}
	payload, err := packBuildContext(df, files)
	if err != nil {
		return BuildResult{}, managerError("构建镜像",
			sandboxerr.WrapKind("image.build", sandboxerr.KindInvalid, err),
			errorContext{Image: tag})
	}
	resp, err := b.client.api.ImageBuild(ctx, bytes.NewReader(payload), build.ImageBuildOptions{
		Tags:        []string{tag},
		Dockerfile:  "Dockerfile",
		NoCache:     spec.NoCache,
		PullParent:  spec.Pull,
		Remove:      true,
		ForceRemove: true,
		BuildArgs:   ptrStringMap(spec.BuildArgs),
		Target:      strings.TrimSpace(spec.Target),
	})
	if err != nil {
		return BuildResult{}, managerError("构建镜像", wrapEngine("image.build", err), errorContext{Image: tag})
	}
	defer resp.Body.Close()
	id, logs, err := parseBuildStream(resp.Body)
	if err != nil {
		return BuildResult{Dockerfile: df, Logs: logs, Tag: tag}, managerError("构建镜像", err, errorContext{Image: tag})
	}
	if id == "" {
		if insp, _, ierr := b.client.api.ImageInspectWithRaw(ctx, tag); ierr == nil {
			id = insp.ID
		}
	}
	return BuildResult{ID: id, Tag: tag, Dockerfile: df, Logs: logs}, nil
}
