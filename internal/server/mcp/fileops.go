package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"connectrpc.com/connect"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 卷里文件的改名、移动、复制、压缩、解压（见 docs/mcp.md#文件）。和网页的文件管理同一组接口，在节点本地进行。
// 同名时默认什么都不改、返回冲突的名字，agent 再选覆盖、保留两者或跳过；覆盖会丢掉目标位置原来的内容，要先预览、
// 用户同意后带确认码执行。

func init() {
	allTools = append(allTools,
		def(tool{name: "move_files", title: "Move, copy or rename files", exec: true, write: true, destructive: true,
			description: "Move or copy files and directories inside one container volume, or rename one in place. When a target name already exists nothing changes and the conflicts are returned; then choose conflict rename (keep both), skip, or overwrite. Overwrite previews first: show it to the user, then call again with the same arguments and confirm set to the code from the preview. Needs the run-commands permission.",
			procedures: []string{nagiv1connect.ProjectServiceListProjectsProcedure, nagiv1connect.ProjectServiceGetProjectProcedure,
				nagiv1connect.ProjectServiceRenameVolumeFileProcedure, nagiv1connect.ProjectServiceMoveVolumeFilesProcedure, nagiv1connect.ProjectServiceCopyVolumeFilesProcedure}},
			toolMoveFiles),
		def(tool{name: "compress_files", title: "Compress files", exec: true, write: true,
			description: "Pack files and directories of one directory in a container volume into a zip or tar.gz archive in the same volume. Symlinks and special files are skipped. Conflicts are handled like move_files. Needs the run-commands permission.",
			procedures:  []string{nagiv1connect.ProjectServiceListProjectsProcedure, nagiv1connect.ProjectServiceGetProjectProcedure, nagiv1connect.ProjectServiceCompressVolumeFilesProcedure}},
			toolCompressFiles),
		def(tool{name: "extract_file", title: "Extract archive", exec: true, write: true, destructive: true,
			description: "Extract a zip, tar.gz (.tgz) or tar archive in a container volume, into a new directory named after the archive (default) or into an existing directory. Conflicts are handled like move_files. Needs the run-commands permission.",
			procedures:  []string{nagiv1connect.ProjectServiceListProjectsProcedure, nagiv1connect.ProjectServiceGetProjectProcedure, nagiv1connect.ProjectServiceExtractVolumeFileProcedure}},
			toolExtractFile),
	)
}

// confirmCode 是两步操作的确认码：由工具和参数算出，预览之后改了任何一项都对不上。和 node_exec 的确认码一样，
// 只保证执行的就是预览过的操作，用户同意靠 agent 和客户端的确认提示。
func confirmCode(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:6])
}

// conflictModes 是同名时的处理：空表示有同名就什么都不改、返回冲突（网页先问用户的那一步）。
var conflictModes = map[string]bool{"": true, "overwrite": true, "rename": true, "skip": true}

func checkConflict(mode string) error {
	if !conflictModes[mode] {
		return errors.New("conflict must be empty, overwrite, rename or skip")
	}
	return nil
}

type fileOpView struct {
	Done         bool     `json:"done"`
	StillRunning bool     `json:"still_running,omitempty"`
	Conflicts    []string `json:"conflicts,omitempty"`
	Paths        []string `json:"paths,omitempty"`
	Skipped      int32    `json:"skipped_existing,omitempty"`
	SkippedLinks int32    `json:"skipped_links,omitempty"`
	Files        int64    `json:"files,omitempty"`
	Bytes        int64    `json:"bytes,omitempty"`
	Note         string   `json:"note,omitempty"`
}

func fileOpOf(r *nagiv1.FileOpResult) fileOpView {
	v := fileOpView{Done: len(r.GetConflicts()) == 0, Conflicts: r.GetConflicts(), Paths: r.GetPaths(), Skipped: r.GetSkipped(), SkippedLinks: r.GetSkippedLinks()}
	if !v.Done {
		v.Note = "these names already exist at the target and nothing was changed; call again with conflict rename (keep both), skip, or overwrite (previews first)"
	}
	if v.SkippedLinks > 0 {
		v.Note = strings.TrimSpace(v.Note + " symlinks, hard links and special files were not processed")
	}
	return v
}

// fileOp 等一个流式的文件操作做完，把进度作为通知发给客户端。和部署一样用不跟着客户端取消的 ctx：开始了就在节点上做完
// （节点先写到临时名字，做完才改名）；超过 deployWait 返回「仍在进行」。
func (c *call) fileOp(open func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.FileOpEvent], error)) (fileOpView, error) {
	stream, err := open(context.WithoutCancel(c.ctx))
	if err != nil {
		return fileOpView{}, err
	}
	type item struct {
		ev  *nagiv1.FileOpEvent
		err error
	}
	ch := make(chan item, 16)
	go func() {
		defer stream.Close()
		defer close(ch)
		for stream.Receive() {
			ch <- item{ev: stream.Msg()}
		}
		if err := stream.Err(); err != nil {
			ch <- item{err: err}
		}
	}()
	var token any
	if c.req != nil && c.req.Params != nil {
		token = c.req.Params.GetProgressToken()
	}
	var files, bytes int64
	timer := time.NewTimer(deployWait)
	defer timer.Stop()
	drain := func() {
		go func() {
			for range ch {
			}
		}()
	}
	for {
		select {
		case it, ok := <-ch:
			if !ok {
				return fileOpView{Done: true, Files: files, Bytes: bytes}, nil
			}
			if it.err != nil {
				drain()
				return fileOpView{}, it.err
			}
			if r := it.ev.GetResult(); r != nil {
				v := fileOpOf(r)
				v.Files, v.Bytes = files, bytes
				drain()
				return v, nil
			}
			files, bytes = it.ev.GetFiles(), it.ev.GetBytes()
			if token != nil && c.req.Session != nil {
				_ = c.req.Session.NotifyProgress(c.ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: float64(files),
					Message: fmt.Sprintf("%d files, %d bytes", files, bytes)})
			}
		case <-timer.C:
			drain()
			return fileOpView{StillRunning: true, Files: files, Bytes: bytes, Note: "still running on the node; check the result with list_files later"}, nil
		case <-c.ctx.Done():
			drain()
			return fileOpView{}, c.ctx.Err()
		}
	}
}

// overwritePreview：覆盖同名会丢掉目标位置原来的内容，先预览。返回 nil 表示可以执行。
func overwritePreview(tool, conflict, confirm, code, text string) map[string]any {
	if conflict != "overwrite" || confirm == code {
		return nil
	}
	out := preview(tool, code, text+" Existing files and directories with the same names at the target are replaced and their old content is lost.", nil)
	if confirm != "" {
		out["note"] = "the confirm code does not match these arguments; show this preview to the user again"
	}
	return out
}

func cleanPaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, errors.New("paths is required")
	}
	if len(paths) > 1000 {
		return nil, errors.New("at most 1000 paths at once")
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.Trim(p, "/")
		if p == "" {
			return nil, errors.New("paths cannot include the volume root")
		}
		out = append(out, p)
	}
	return out, nil
}

type MoveFilesArgs struct {
	ContainerArg
	Volume string   `json:"volume" jsonschema:"volume name from list_files; .data is the container's data/ directory"`
	Paths  []string `json:"paths" jsonschema:"files and directories to move or copy, relative to the volume root; exactly one for rename"`
	Action string   `json:"action" jsonschema:"move, copy or rename"`
	// 移动、复制的目标目录；改名时不用填（在原来的目录里改名）。
	DestDir  string `json:"dest_dir,omitempty" jsonschema:"move and copy: target directory relative to the volume root, empty for the root; it must exist"`
	NewName  string `json:"new_name,omitempty" jsonschema:"rename: the new name (no /)"`
	Conflict string `json:"conflict,omitempty" jsonschema:"when a target name exists: empty (default) changes nothing and returns the conflicts; rename keeps both; skip; overwrite replaces the target (previews first)"`
	Confirm  string `json:"confirm,omitempty" jsonschema:"with conflict overwrite: omit to get a preview, then the code from the preview after the user agreed"`
}

func toolMoveFiles(c *call, in MoveFilesArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	paths, err := cleanPaths(in.Paths)
	if err != nil {
		return nil, err
	}
	if err := checkConflict(in.Conflict); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Volume) == "" {
		return nil, errors.New("volume is required")
	}
	req := &nagiv1.VolumeTransferRequest{ProjectId: s.GetProject().GetId(), Container: m.GetName(), Volume: in.Volume, Paths: paths,
		DestDir: strings.Trim(in.DestDir, "/"), Conflict: in.Conflict}
	action := strings.ToLower(in.Action)
	switch action {
	case "rename":
		if len(paths) != 1 || strings.TrimSpace(in.NewName) == "" || strings.Contains(in.NewName, "/") {
			return nil, errors.New("rename needs exactly one path and a new_name without /")
		}
		req.NewName, req.DestDir = in.NewName, strings.TrimPrefix(path.Dir("/"+paths[0]), "/")
	case "move", "copy":
	default:
		return nil, errors.New("action must be move, copy or rename")
	}
	code := confirmCode("move_files", req.GetProjectId(), req.GetContainer(), req.GetVolume(), action, strings.Join(paths, "\x01"), req.GetDestDir(), req.GetNewName())
	if p := overwritePreview("move_files", in.Conflict, in.Confirm, code, fmt.Sprintf("%s %s in volume %s of %s to %s.", action, strings.Join(paths, ", "), in.Volume, m.GetName(), "/"+req.GetDestDir())); p != nil {
		return p, nil
	}
	if action == "rename" {
		res, err := c.h.clients.projects.RenameVolumeFile(c.ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return fileOpOf(res.Msg), nil
	}
	return c.fileOp(func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.FileOpEvent], error) {
		if action == "copy" {
			return c.h.clients.projects.CopyVolumeFiles(ctx, connect.NewRequest(req))
		}
		return c.h.clients.projects.MoveVolumeFiles(ctx, connect.NewRequest(req))
	})
}

type CompressFilesArgs struct {
	ContainerArg
	Volume   string   `json:"volume" jsonschema:"volume name from list_files"`
	Paths    []string `json:"paths" jsonschema:"files and directories to pack, all in the same directory, relative to the volume root"`
	Dest     string   `json:"dest" jsonschema:"archive path relative to the volume root, e.g. backup/site.tar.gz"`
	Format   string   `json:"format,omitempty" jsonschema:"zip or tar.gz; default from the dest extension, otherwise tar.gz"`
	Conflict string   `json:"conflict,omitempty" jsonschema:"when dest exists: empty (default) changes nothing; rename; skip; overwrite (previews first)"`
	Confirm  string   `json:"confirm,omitempty" jsonschema:"with conflict overwrite: the code from the preview"`
}

func archiveFormat(format, dest string) (string, error) {
	switch strings.ToLower(format) {
	case "zip":
		return "zip", nil
	case "tar.gz", "tgz":
		return "tar.gz", nil
	case "":
		if strings.HasSuffix(strings.ToLower(dest), ".zip") {
			return "zip", nil
		}
		return "tar.gz", nil
	}
	return "", errors.New("format must be zip or tar.gz")
}

// archiveBase 是压缩包去掉后缀的名字，解压到新目录时用它做目录名（和网页的 archiveBase 一样）。
func archiveBase(name string) string {
	lower := strings.ToLower(name)
	for _, ext := range []string{".tar.gz", ".tgz", ".zip", ".tar"} {
		if strings.HasSuffix(lower, ext) && len(lower) > len(ext) {
			return name[:len(name)-len(ext)]
		}
	}
	return name
}

func toolCompressFiles(c *call, in CompressFilesArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	paths, err := cleanPaths(in.Paths)
	if err != nil {
		return nil, err
	}
	if err := checkConflict(in.Conflict); err != nil {
		return nil, err
	}
	dest := strings.Trim(in.Dest, "/")
	if dest == "" || strings.TrimSpace(in.Volume) == "" {
		return nil, errors.New("volume and dest are required")
	}
	format, err := archiveFormat(in.Format, dest)
	if err != nil {
		return nil, err
	}
	req := &nagiv1.VolumeCompressRequest{ProjectId: s.GetProject().GetId(), Container: m.GetName(), Volume: in.Volume, Paths: paths, Dest: dest, Format: format, Conflict: in.Conflict}
	code := confirmCode("compress_files", req.GetProjectId(), req.GetContainer(), req.GetVolume(), strings.Join(paths, "\x01"), dest, format)
	if p := overwritePreview("compress_files", in.Conflict, in.Confirm, code, fmt.Sprintf("pack %s in volume %s of %s into %s.", strings.Join(paths, ", "), in.Volume, m.GetName(), dest)); p != nil {
		return p, nil
	}
	return c.fileOp(func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.FileOpEvent], error) {
		return c.h.clients.projects.CompressVolumeFiles(ctx, connect.NewRequest(req))
	})
}

type ExtractFileArgs struct {
	ContainerArg
	Volume string `json:"volume" jsonschema:"volume name from list_files"`
	Path   string `json:"path" jsonschema:"the archive (zip, tar.gz, .tgz or tar), relative to the volume root"`
	// 默认解压到压缩包所在的目录。
	DestDir string `json:"dest_dir,omitempty" jsonschema:"directory to extract in, relative to the volume root; default the archive's directory"`
	// 默认新建以压缩包命名的目录（和网页默认一样），不会和已有的文件混在一起。
	Merge    bool   `json:"merge,omitempty" jsonschema:"extract the archive's top-level entries directly into dest_dir instead of a new directory named after the archive"`
	Conflict string `json:"conflict,omitempty" jsonschema:"when a target name exists: empty (default) changes nothing; rename; skip; overwrite (previews first)"`
	Confirm  string `json:"confirm,omitempty" jsonschema:"with conflict overwrite: the code from the preview"`
}

func toolExtractFile(c *call, in ExtractFileArgs) (any, error) {
	s, m, err := c.container(in.ContainerArg)
	if err != nil {
		return nil, err
	}
	if err := checkConflict(in.Conflict); err != nil {
		return nil, err
	}
	p := strings.Trim(in.Path, "/")
	if p == "" || strings.TrimSpace(in.Volume) == "" {
		return nil, errors.New("volume and path are required")
	}
	dest := strings.Trim(in.DestDir, "/")
	if in.DestDir == "" {
		dest = strings.TrimPrefix(path.Dir("/"+p), "/")
	}
	// 解压到新目录时，接口的 dest_dir 是新目录本身（和网页一样以压缩包去掉后缀命名）。
	if !in.Merge {
		dest = strings.TrimPrefix(path.Join(dest, archiveBase(path.Base(p))), "/")
	}
	req := &nagiv1.VolumeExtractRequest{ProjectId: s.GetProject().GetId(), Container: m.GetName(), Volume: in.Volume, Path: p, DestDir: dest, IntoNewDir: !in.Merge, Conflict: in.Conflict}
	code := confirmCode("extract_file", req.GetProjectId(), req.GetContainer(), req.GetVolume(), p, dest, fmt.Sprint(in.Merge))
	if pv := overwritePreview("extract_file", in.Conflict, in.Confirm, code, fmt.Sprintf("extract %s in volume %s of %s into %s.", p, in.Volume, m.GetName(), "/"+dest)); pv != nil {
		return pv, nil
	}
	return c.fileOp(func(ctx context.Context) (*connect.ServerStreamForClient[nagiv1.FileOpEvent], error) {
		return c.h.clients.projects.ExtractVolumeFile(ctx, connect.NewRequest(req))
	})
}
