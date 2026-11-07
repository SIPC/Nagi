package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SIPC/nagi/internal/agent/nodefiles"
	"github.com/SIPC/nagi/internal/pkg/mesh"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 节点访问：节点的终端和文件（见 docs/node-access.md）。

const (
	// maxNodeShells 是一个节点同时打开的终端数。
	maxNodeShells = 8
	// nodeShellMaxAge 是终端最长的使用时间。面板还会按没有输入输出的时长断开。
	nodeShellMaxAge = 12 * time.Hour
)

var activeNodeShells atomic.Int32

func nodeAccessFile(dataDir string) string {
	return filepath.Join(dataDir, "identity", "node-access-disabled")
}

// NodeAccessDisabled 判断节点是否在本地关闭了节点访问。每次都读文件：本地命令执行后立即生效。
func NodeAccessDisabled(dataDir string) bool {
	_, err := os.Stat(nodeAccessFile(dataDir))
	return err == nil
}

// SetNodeAccess 打开或关闭节点访问。只有节点本地命令调用：面板没有接口能改它，所以面板被攻破也打不开。
func SetNodeAccess(dataDir string, enabled bool) error {
	file := nodeAccessFile(dataDir)
	if enabled {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	return os.WriteFile(file, nil, 0o600)
}

// nodeAccessSupported：只在 Linux 上提供。
func nodeAccessSupported() bool { return runtime.GOOS == "linux" }

func inContainer() bool {
	for _, f := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

// checkNodeAccess 是所有节点访问接口的第一步。
func (a *agent) checkNodeAccess() error {
	if !nodeAccessSupported() {
		return status.Error(codes.Unimplemented, "此节点的系统不支持节点访问")
	}
	if NodeAccessDisabled(a.cfg.DataDir) {
		return status.Error(codes.FailedPrecondition, "节点访问已关闭。请在节点上执行 nagi node access enable 开启")
	}
	return nil
}

// newNodeFiles 建节点文件的服务。Nagi 的数据目录禁止访问；面板自带的节点，面板的数据目录在上一层，也算。
func (a *agent) newNodeFiles() *nodefiles.Service {
	dirs := []string{a.cfg.DataDir}
	if a.cfg.Embedded {
		dirs = append(dirs, filepath.Dir(a.cfg.DataDir))
	}
	return &nodefiles.Service{
		Denied:     dirs,
		Managed:    func() []string { return a.projects.Dirs(context.Background()) },
		BackupDir:  filepath.Join(a.cfg.DataDir, "file-backups"),
		StagingDir: filepath.Join(a.cfg.DataDir, "node-files-staging"),
	}
}

// nodeExecMax 是节点命令最长的执行时间，面板给的超时不会超过它。
const nodeExecMax = 10 * time.Minute

// streamWriter 把命令的输出按块发给面板。
type streamWriter struct {
	mu   sync.Mutex
	send func(*nagiv1.ExecOutput) error
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.send(&nagiv1.ExecOutput{Output: &nagiv1.ExecOutput_Stdout{Stdout: append([]byte(nil), p...)}}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// NodeExec 在节点上执行一条命令（MCP 的 node_exec）。和终端共用节点访问的开关和数量上限，也记一条本地日志。
func (s *agentService) NodeExec(req *nagiv1.NodeExecRequest, stream nagiv1.AgentService_NodeExecServer) error {
	if err := s.node.checkNodeAccess(); err != nil {
		return err
	}
	if req.GetCommand() == "" {
		return status.Error(codes.InvalidArgument, "命令不能为空")
	}
	if activeNodeShells.Add(1) > maxNodeShells {
		activeNodeShells.Add(-1)
		return status.Errorf(codes.ResourceExhausted, "终端数量已达上限（%d 个），请关闭不用的终端", maxNodeShells)
	}
	defer activeNodeShells.Add(-1)
	timeout := min(time.Duration(req.GetTimeoutSeconds())*time.Second, nodeExecMax)
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(stream.Context(), timeout)
	defer cancel()
	// 面板的记录被篡改时还能在节点上查。命令本身也记下。
	log := s.node.log.With("panel", s.panel, "user", req.GetActor())
	log.Info("在节点上执行命令", "command", req.GetCommand())
	code := runNodeCommand(ctx, req.GetCwd(), req.GetCommand(), &streamWriter{send: stream.Send})
	log.Info("节点命令结束", "exit_code", code)
	return stream.Send(&nagiv1.ExecOutput{Output: &nagiv1.ExecOutput_ExitCode{ExitCode: int32(code)}})
}

func (s *agentService) NodeShell(stream nagiv1.AgentService_NodeShellServer) error {
	if err := s.node.checkNodeAccess(); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return status.Error(codes.InvalidArgument, "第一条消息必须是 start")
	}
	if activeNodeShells.Add(1) > maxNodeShells {
		activeNodeShells.Add(-1)
		return status.Errorf(codes.ResourceExhausted, "终端数量已达上限（%d 个），请关闭不用的终端", maxNodeShells)
	}
	defer activeNodeShells.Add(-1)

	size := start.GetSize()
	sh, err := startShell(start.GetCwd(), uint16(size.GetCols()), uint16(size.GetRows()))
	if err != nil {
		return status.Errorf(codes.Internal, "无法启动终端：%v", err)
	}
	// 面板的记录被篡改时还能在节点上查。用户名是面板说的，指纹是控制连接确认过的。
	log := s.node.log.With("panel", s.panel, "user", start.GetActor())
	log.Info("打开了节点终端")
	began := time.Now()
	defer func() { log.Info("关闭了节点终端", "duration", time.Since(began).Round(time.Second).String()) }()
	defer sh.Close()

	ctx, cancel := context.WithTimeout(stream.Context(), nodeShellMaxAge)
	defer cancel()
	// 对方走了、时间到了：关掉 PTY，下面的读就结束了。
	go func() {
		<-ctx.Done()
		sh.Close()
	}()
	go func() {
		defer cancel()
		for {
			in, err := stream.Recv()
			if err != nil {
				return
			}
			switch v := in.Input.(type) {
			case *nagiv1.NodeShellInput_Stdin:
				if _, err := sh.Write(v.Stdin); err != nil {
					return
				}
			case *nagiv1.NodeShellInput_Resize:
				sh.Resize(uint16(v.Resize.GetCols()), uint16(v.Resize.GetRows()))
			}
		}
	}()

	buf := make([]byte, 32*1024)
	for {
		n, err := sh.Read(buf)
		if n > 0 {
			if err := stream.Send(&nagiv1.ExecOutput{Output: &nagiv1.ExecOutput_Stdout{Stdout: append([]byte(nil), buf[:n]...)}}); err != nil {
				return err
			}
		}
		if err != nil {
			break
		}
	}
	return stream.Send(&nagiv1.ExecOutput{Output: &nagiv1.ExecOutput_ExitCode{ExitCode: int32(sh.Wait())}})
}

func (s *agentService) ListNodeDir(_ context.Context, req *nagiv1.NodeFileRef) (*nagiv1.ListNodeDirResponse, error) {
	if err := s.node.checkNodeAccess(); err != nil {
		return nil, err
	}
	return s.node.files.List(req.GetPath())
}

func (s *agentService) ReadNodeFile(_ context.Context, req *nagiv1.NodeFileRef) (*nagiv1.ReadNodeFileResponse, error) {
	if err := s.node.checkNodeAccess(); err != nil {
		return nil, err
	}
	return s.node.files.Read(req.GetPath())
}

func (s *agentService) WriteNodeFile(_ context.Context, req *nagiv1.WriteNodeFileRequest) (*nagiv1.WriteNodeFileResponse, error) {
	if err := s.node.checkNodeAccess(); err != nil {
		return nil, err
	}
	return s.node.files.Write(req)
}

func (s *agentService) DeleteNodeFile(_ context.Context, req *nagiv1.DeleteNodeFileRequest) (*nagiv1.NodeFileOpResponse, error) {
	if err := s.node.checkNodeAccess(); err != nil {
		return nil, err
	}
	if err := s.node.files.Delete(req); err != nil {
		return nil, err
	}
	return &nagiv1.NodeFileOpResponse{}, nil
}

func (s *agentService) CreateNodeDir(_ context.Context, req *nagiv1.NodeFileRef) (*nagiv1.NodeFileOpResponse, error) {
	if err := s.node.checkNodeAccess(); err != nil {
		return nil, err
	}
	if err := s.node.files.Mkdir(req.GetPath()); err != nil {
		return nil, err
	}
	return &nagiv1.NodeFileOpResponse{}, nil
}

func (s *agentService) ListNodeFileBackups(_ context.Context, req *nagiv1.NodeFileRef) (*nagiv1.ListNodeFileBackupsResponse, error) {
	if err := s.node.checkNodeAccess(); err != nil {
		return nil, err
	}
	return s.node.files.Backups(req.GetPath())
}

func (s *agentService) RestoreNodeFileBackup(_ context.Context, req *nagiv1.RestoreNodeFileBackupRequest) (*nagiv1.WriteNodeFileResponse, error) {
	if err := s.node.checkNodeAccess(); err != nil {
		return nil, err
	}
	return s.node.files.Restore(req)
}

// panelOf 返回 mesh 公钥属于哪个面板。
func (n *meshNode) panelOf(k mesh.Key) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for fp, pk := range n.panelKeys {
		if pk == k {
			return fp, true
		}
	}
	return "", false
}

// checkMeshNodeAccess 检查 mesh 上传来的节点文件请求：mesh 上有别的节点，只接受主面板。primaryGuard 只挂在控制连接上，这里要自己查。
func (s *meshService) checkMeshNodeAccess(ctx context.Context, method string) error {
	caller, ok := mesh.Caller(ctx, s.M)
	if !ok {
		return status.Error(codes.PermissionDenied, "仅接受受信任的面板")
	}
	fp, ok := s.a.mesh.panelOf(caller)
	if !ok {
		return status.Error(codes.PermissionDenied, "仅接受受信任的面板")
	}
	if err := s.a.checkPrimary(fp, method); err != nil {
		return err
	}
	return s.a.checkNodeAccess()
}

// NodeFileDownload 把节点上的文件发给面板。
func (s *meshService) NodeFileDownload(req *nagiv1.NodeFileRef, stream nagiv1.MeshService_NodeFileDownloadServer) error {
	if err := s.checkMeshNodeAccess(stream.Context(), nagiv1.MeshService_NodeFileDownload_FullMethodName); err != nil {
		return err
	}
	return s.a.files.Download(stream.Context(), req.GetPath(), stream.Send)
}

// NodeFileUpload 把面板传来的文件写到节点上。
func (s *meshService) NodeFileUpload(stream nagiv1.MeshService_NodeFileUploadServer) error {
	if err := s.checkMeshNodeAccess(stream.Context(), nagiv1.MeshService_NodeFileUpload_FullMethodName); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	ref := first.GetRef()
	if err := s.a.files.CheckUpload(ref.GetPath(), first.GetConfirmed()); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() {
		for {
			c, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(c.GetData()); err != nil {
				return
			}
		}
	}()
	n, err := s.a.files.Upload(ref.GetPath(), first.GetConfirmed(), pr)
	pr.Close()
	if err != nil {
		return err
	}
	return stream.SendAndClose(&nagiv1.VolumeUploadResponse{Size: n})
}
