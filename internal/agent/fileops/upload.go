package fileops

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/SIPC/nagi/internal/agent/metrics"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 分块上传（断点续传，见 docs/agent.md#存储）。每个上传在暂存目录里有一个子目录：data 是已经收到的内容，
// meta.json 记着目标、大小、已经核对过的长度和到这里为止的 SHA-256 中间状态，所以节点重启以后也能接着传。
// 收齐并核对过整个文件的 SHA-256 以后，调用方才把 data 移到目标位置。

const (
	// MaxChunk 是一块的上限。界面每块 8 MiB。
	MaxChunk = 16 << 20
	// UploadKeep：超过这么久没有动静的上传当作放弃了，下次开始上传时删掉。
	UploadKeep = 7 * 24 * time.Hour
)

var uploadIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

var (
	errUploadGone = status.Error(codes.NotFound, "上传已失效，请重新上传")
	// ErrUploadOffset：块的起点和节点已经收到的长度不一致（例如上一块其实已经收到了），界面重新开始一次拿到正确的位置。
	ErrUploadOffset = status.Error(codes.Aborted, "上传位置不一致，请重试")
	errUploadBusy   = status.Error(codes.Aborted, "这个文件正在别处上传")
)

// Uploads 管理一个暂存目录里的分块上传。
type Uploads struct {
	Dir string

	mu   sync.Mutex
	busy map[string]bool
}

type uploadMeta struct {
	// Target 是调用方给的目标标识（卷里的路径，或者节点上的真实路径），提交时核对，上传不能挪到别处用。
	Target  string    `json:"target"`
	Key     string    `json:"key"`
	Size    int64     `json:"size"`
	Offset  int64     `json:"offset"`
	Hash    []byte    `json:"hash"`
	Updated time.Time `json:"updated"`
}

// UploadInfo 是上传的当前状态。
type UploadInfo struct {
	ID     string
	Offset int64
	Size   int64
}

func (u *Uploads) dir(id string) string { return filepath.Join(u.Dir, id) }

// acquire 标记上传正在进行，同一个上传的两块不能同时写（例如两个标签页传同一个文件）。
func (u *Uploads) acquire(id string) (func(), error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.busy == nil {
		u.busy = map[string]bool{}
	}
	if u.busy[id] {
		return nil, errUploadBusy
	}
	u.busy[id] = true
	return func() {
		u.mu.Lock()
		delete(u.busy, id)
		u.mu.Unlock()
	}, nil
}

func (u *Uploads) load(id string) (*uploadMeta, error) {
	if !uploadIDRe.MatchString(id) {
		return nil, errUploadGone
	}
	b, err := os.ReadFile(filepath.Join(u.dir(id), "meta.json"))
	if err != nil {
		return nil, errUploadGone
	}
	var m uploadMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, errUploadGone
	}
	return &m, nil
}

func (u *Uploads) save(id string, m *uploadMeta) error {
	m.Updated = time.Now()
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := filepath.Join(u.dir(id), "meta.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(u.dir(id), "meta.json"))
}

// Begin 开始或接着一个上传：同一个目标、同样大小和标识的上传还在时返回它和已经收到的长度；
// 同一个目标的其他上传（换了文件）删掉。顺便清掉放弃了的上传。磁盘放不下剩下的部分时拒绝。
func (u *Uploads) Begin(target, key string, size int64) (UploadInfo, error) {
	if size < 0 || key == "" || len(key) > 512 {
		return UploadInfo{}, invalid("上传参数无效")
	}
	if err := os.MkdirAll(u.Dir, 0o700); err != nil {
		return UploadInfo{}, err
	}
	entries, _ := os.ReadDir(u.Dir)
	for _, e := range entries {
		if !uploadIDRe.MatchString(e.Name()) {
			continue
		}
		m, err := u.load(e.Name())
		if err != nil {
			// 没写完 meta 的残留：放了很久才删。
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > UploadKeep {
				_ = os.RemoveAll(u.dir(e.Name()))
			}
			continue
		}
		switch {
		case m.Target == target && m.Key == key && m.Size == size:
			if err := metrics.CheckDisk(u.Dir, size-m.Offset); err != nil {
				return UploadInfo{}, err
			}
			return UploadInfo{ID: e.Name(), Offset: m.Offset, Size: size}, nil
		case m.Target == target || time.Since(m.Updated) > UploadKeep:
			if release, err := u.acquire(e.Name()); err == nil {
				_ = os.RemoveAll(u.dir(e.Name()))
				release()
			}
		}
	}
	if err := metrics.CheckDisk(u.Dir, size); err != nil {
		return UploadInfo{}, err
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	if err := os.Mkdir(u.dir(id), 0o700); err != nil {
		return UploadInfo{}, err
	}
	f, err := os.OpenFile(filepath.Join(u.dir(id), "data"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return UploadInfo{}, err
	}
	f.Close()
	state, _ := sha256.New().(encoding.BinaryMarshaler).MarshalBinary()
	if err := u.save(id, &uploadMeta{Target: target, Key: key, Size: size, Hash: state}); err != nil {
		_ = os.RemoveAll(u.dir(id))
		return UploadInfo{}, err
	}
	return UploadInfo{ID: id, Size: size}, nil
}

func restoreHash(state []byte) (hash.Hash, error) {
	h := sha256.New()
	if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
		return nil, errUploadGone
	}
	return h, nil
}

// Append 把从 offset 开始的一块写进上传。offset 必须正好是已经收到的长度；块的 SHA-256 对不上时丢掉这一块。
// 返回新的长度。上一次写到一半（节点在写入中途重启）多出来的部分先截掉。
func (u *Uploads) Append(id, target string, offset int64, sum string, r io.Reader) (int64, error) {
	release, err := u.acquire(id)
	if err != nil {
		return 0, err
	}
	defer release()
	m, err := u.load(id)
	if err != nil {
		return 0, err
	}
	if m.Target != target {
		return 0, errUploadGone
	}
	if offset != m.Offset {
		return m.Offset, ErrUploadOffset
	}
	chunk, err := io.ReadAll(io.LimitReader(r, MaxChunk+1))
	if err != nil {
		return 0, err
	}
	if len(chunk) > MaxChunk {
		return 0, invalid("一块不能超过 16 MiB")
	}
	if m.Offset+int64(len(chunk)) > m.Size {
		return 0, invalid("上传的内容超过了文件大小")
	}
	got := sha256.Sum256(chunk)
	if !strings.EqualFold(hex.EncodeToString(got[:]), sum) {
		return m.Offset, status.Error(codes.DataLoss, "这一块传输中出错，请重试")
	}
	if err := metrics.CheckDisk(u.Dir, int64(len(chunk))); err != nil {
		return 0, err
	}
	h, err := restoreHash(m.Hash)
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(filepath.Join(u.dir(id), "data"), os.O_WRONLY, 0)
	if err != nil {
		return 0, errUploadGone
	}
	err = f.Truncate(m.Offset)
	if err == nil {
		_, err = f.WriteAt(chunk, m.Offset)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	h.Write(chunk)
	if m.Hash, err = h.(encoding.BinaryMarshaler).MarshalBinary(); err != nil {
		return 0, err
	}
	m.Offset += int64(len(chunk))
	if err := u.save(id, m); err != nil {
		return 0, err
	}
	return m.Offset, nil
}

// Finish 核对上传收齐了、整个文件的 SHA-256 和 sum 一致，返回内容所在的文件。调用方把它移走以后调用 Remove。
// 校验不一致时上传作废。返回的 release 在调用方处理完以前占着这个上传。
func (u *Uploads) Finish(id, target, sum string) (data string, size int64, release func(), err error) {
	release, err = u.acquire(id)
	if err != nil {
		return "", 0, nil, err
	}
	m, err := u.load(id)
	if err == nil && m.Target != target {
		err = errUploadGone
	}
	if err == nil && m.Offset != m.Size {
		err = invalid("文件还没有传完")
	}
	if err != nil {
		release()
		return "", 0, nil, err
	}
	h, err := restoreHash(m.Hash)
	if err != nil {
		release()
		return "", 0, nil, err
	}
	data = filepath.Join(u.dir(id), "data")
	if info, err := os.Stat(data); err != nil || info.Size() < m.Size {
		release()
		_ = os.RemoveAll(u.dir(id))
		return "", 0, nil, errUploadGone
	}
	_ = os.Truncate(data, m.Size)
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), sum) {
		_ = os.RemoveAll(u.dir(id))
		release()
		return "", 0, nil, status.Error(codes.DataLoss, "文件校验不一致，请重新上传")
	}
	return data, m.Size, release, nil
}

// Remove 删掉一个上传（放弃，或者内容已经移走）。target 不一致时不删。
func (u *Uploads) Remove(id, target string) error {
	m, err := u.load(id)
	if err != nil {
		return nil // 已经没有了
	}
	if m.Target != target {
		return errUploadGone
	}
	release, err := u.acquire(id)
	if err != nil {
		return err
	}
	defer release()
	return os.RemoveAll(u.dir(id))
}

// RemoveFinished 在 Finish 之后删掉上传（调用方已经占着它）。
func (u *Uploads) RemoveFinished(id string) {
	if uploadIDRe.MatchString(id) {
		_ = os.RemoveAll(u.dir(id))
	}
}

// Info 返回上传的当前状态（核对目标）。
func (u *Uploads) Info(id, target string) (UploadInfo, error) {
	m, err := u.load(id)
	if err != nil {
		return UploadInfo{}, err
	}
	if m.Target != target {
		return UploadInfo{}, errUploadGone
	}
	return UploadInfo{ID: id, Offset: m.Offset, Size: m.Size}, nil
}
