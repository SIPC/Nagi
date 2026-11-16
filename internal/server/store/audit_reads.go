package store

// SensitiveReads 是要记进审计的查询（见 docs/audit.md#记录范围）：返回密钥、变量值或文件内容的接口。
// api 包的测试列出所有返回这类内容的接口，没登记在这里、也没登记为不用记的，测试失败。
var SensitiveReads = map[string]bool{
	// 项目配置里有环境变量和共享变量的值。界面会定时刷新，每个会话、每个项目每小时只记一条。
	"GetProject":   true,
	"ListProjects": true,
	// 容器的备份密钥。
	"GetBackupKey": true,
	// 告警渠道的地址和 Telegram 令牌。
	"GetAlertConfig": true,
	// 节点上的文件内容。
	"ReadNodeFile": true,
	// 不走 Connect 的下载（/api/volume-file、/api/node-file）由处理函数按这两个名字记。
	"DownloadVolumeFile": true,
	"DownloadNodeFile":   true,
	// 回放终端录制。
	"GetRecording": true,
}

// SessionActions 是终端和命令执行的记录（类别 session）。
var SessionActions = map[string]bool{
	"OpenNodeShell": true, "CloseNodeShell": true, "OpenTerminal": true, "CloseTerminal": true,
	"ExecCommand": true, "NodeExec": true,
}
