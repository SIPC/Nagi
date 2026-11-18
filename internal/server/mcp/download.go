package mcp

import (
	"context"
	"errors"
	"strings"

	"github.com/SIPC/nagi/internal/pkg/directdl"
	nagiv1 "github.com/SIPC/nagi/internal/proto/nagi/v1"
	"github.com/SIPC/nagi/internal/proto/nagi/v1/nagiv1connect"
	"github.com/SIPC/nagi/internal/server/api"
	"github.com/SIPC/nagi/internal/server/auth"
)

// 直连下载的链接和命令（见 docs/agent.md#直连下载、docs/mcp.md#文件）：给管理员把大文件交给别的机器或下载工具。
// 规则和网页的菜单一样：节点开着直连下载时 HTTPS 优先，HTTPS 不能用时给加密 HTTP 下载的命令，都不能用时报错。
// 权限和读文件相同：卷文件同 read_file（令牌要允许执行命令），节点文件同 read_node_file（令牌要勾「节点终端」），
// 另外只给管理员。签发照常记一次下载的活动记录；返回的命令里有解密的密钥，只在结果里，不进活动记录。

// DirectAccess 是签发直连下载的接口（api.API.VolumeDirectLink、NodeDirectLink），按 ctx 里的用户（auth.WithUser）检查权限。
// 和 FileAccess 一样不经过接口的拦截器，调用前要把令牌的用户放进 ctx。
type DirectAccess struct {
	Volume func(ctx context.Context, ref *nagiv1.VolumeFileRef) (api.DirectResult, error)
	Node   func(ctx context.Context, nodeID, path string) (api.DirectResult, error)
}

const (
	linkNote    = "anyone with this link can download the file within 5 minutes (downloads already started finish); share it only with the intended recipient"
	commandNote = "this node has no IP certificate, so the file is served over plain HTTP, encrypted. The command contains the decryption key, which is now part of this conversation and ends up in the shell history where it runs: treat it like a password. command_posix needs sh, curl and openssl (Linux, macOS); command_windows is a PowerShell script for Windows 10/11 (built-in curl.exe, no OpenSSL; paste it into PowerShell, not cmd.exe). Large files download over 4 connections (N at the top of the script). It is valid for 5 minutes, and works only from the first machine (source address) that uses it. If the file changes during the download, the download stops; get a new command"
)

func init() {
	ln := nagiv1connect.NodeServiceListNodesProcedure
	allTools = append(allTools,
		def(tool{name: "get_download_link", title: "Get download link", exec: true, adminOnly: true,
			description: "Administrators only, with the run-commands permission. Get a link to download a file or directory (directories as .tar.gz) of a container volume straight from its node, for another machine or a download manager: an HTTPS link valid for 5 minutes when the node has an IP certificate; otherwise, when the node only has the encrypted HTTP fallback, a command that downloads and decrypts it, for Linux/macOS (curl + openssl) and for Windows (PowerShell). The command contains the decryption key, so it ends up in this conversation: only hand it to the user. Fails when the node's direct download is off or unavailable; use read_file for small files. Recorded like a download.",
			procedures:  []string{nagiv1connect.ProjectServiceListProjectsProcedure, nagiv1connect.ProjectServiceGetProjectProcedure}},
			toolGetDownloadLink),
		def(tool{name: "get_node_download_link", title: "Get node download link", exec: true, nodeExec: true, adminOnly: true,
			description: "Administrators with the node terminal permission only. Like get_download_link, for a file or directory on a node's file system (the paths read_node_file can read). The command for the encrypted HTTP fallback contains the decryption key and ends up in this conversation.",
			procedures:  []string{ln}},
			toolGetNodeDownloadLink),
	)
}

type DownloadLinkArgs struct {
	FileArg
	SnapshotID string `json:"snapshot_id,omitempty" jsonschema:"download the file as it is in this snapshot (see list_snapshots)"`
}

func toolGetDownloadLink(c *call, in DownloadLinkArgs) (any, error) {
	if c.h.o.Direct.Volume == nil {
		return nil, errors.New("not available on this panel")
	}
	ref, err := c.fileRef(in.FileArg)
	if err != nil {
		return nil, err
	}
	ref.SnapshotId = strings.TrimSpace(in.SnapshotID)
	res, err := c.h.o.Direct.Volume(auth.WithUser(c.ctx, c.u), ref)
	if err != nil {
		return nil, err
	}
	return directView(res, ref.GetPath()), nil
}

func toolGetNodeDownloadLink(c *call, in NodePathArgs) (any, error) {
	if c.h.o.Direct.Node == nil {
		return nil, errors.New("not available on this panel")
	}
	_, n, err := c.node(in.Node)
	if err != nil {
		return nil, err
	}
	p, err := nodePath(in.Path)
	if err != nil {
		return nil, err
	}
	res, err := c.h.o.Direct.Node(auth.WithUser(c.ctx, c.u), n.GetId(), p)
	if err != nil {
		return nil, err
	}
	v := directView(res, p)
	v["node"] = n.GetName()
	return v, nil
}

func directView(res api.DirectResult, path string) map[string]any {
	if res.URL != "" {
		return map[string]any{"path": path, "method": "https", "url": res.URL, "expires_in_seconds": int(directdl.TTL.Seconds()), "note": linkNote}
	}
	cmd := res.Command
	out := map[string]any{"path": path, "method": "encrypted_http", "command_posix": cmd.Command, "command_windows": cmd.CommandWindows, "file_name": cmd.Name,
		"archive": cmd.Archive, "expires_in_seconds": int(directdl.TTL.Seconds()), "note": commandNote}
	if !cmd.Archive {
		out["size_bytes"] = cmd.Size
		out["resumable"] = true
	}
	if cmd.SHA256 != "" {
		out["sha256"] = cmd.SHA256
	} else {
		out["not_verified"] = cmd.HashSkipped // too_large、timeout、archive：命令不核对内容
	}
	return out
}
