package logstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const GiB int64 = 1 << 30

// Config 是本机 logs/config.json 的配置。面板和它的内嵌节点共用一份额度。
// 修改后重启生效；包含接入的旧历史库，不包含业务库和 Docker 自己的轮换文件。
type Config struct {
	CapacityGiB             int    `json:"capacity_gib"`
	MetricsEverySeconds     int    `json:"metrics_every_seconds"`
	NodeDetails             bool   `json:"node_details"`
	ContainerRescanSeconds  int    `json:"container_rescan_seconds"`
	ContainerRetrySeconds   int    `json:"container_retry_seconds"`
	ContainerLineBytes      int    `json:"container_line_bytes"`
	TransferProgressSeconds int    `json:"transfer_progress_seconds"`
	RecordingFlushSeconds   int    `json:"recording_flush_seconds"`
	RecentCapacityMiB       int    `json:"recent_capacity_mib"`
	RecentHours             int    `json:"recent_hours"`
	CopyImportantSeconds    int    `json:"copy_important_seconds"`
	CopyOrdinarySeconds     int    `json:"copy_ordinary_seconds"`
	RuntimeLevel            string `json:"runtime_level"`
}

func DefaultConfig() Config {
	return Config{CapacityGiB: 5, MetricsEverySeconds: 10, NodeDetails: true, ContainerRescanSeconds: 15, ContainerRetrySeconds: 5, ContainerLineBytes: 8192, TransferProgressSeconds: 5, RecordingFlushSeconds: 5, RecentCapacityMiB: 1024, RecentHours: 24, CopyImportantSeconds: 1, CopyOrdinarySeconds: 10, RuntimeLevel: "INFO"}
}

// LoadConfig 读取本机配置，不存在时用默认值；无效配置不悄悄回退。
func LoadConfig(dir string) (Config, error) {
	c := DefaultConfig()
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil && !os.IsNotExist(err) {
		return c, err
	}
	if err == nil {
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("日志配置: %w", err)
		}
	}
	if c.CapacityGiB < 5 || c.CapacityGiB > 10 {
		return c, fmt.Errorf("capacity_gib 必须在 5～10 之间")
	}
	// 现有指标采集每 10 秒一次。这里只调日志入库频率，不改变告警采样和旧图表的统计契约。
	if c.MetricsEverySeconds < 10 || c.MetricsEverySeconds > 86400 || c.MetricsEverySeconds%10 != 0 {
		return c, fmt.Errorf("metrics_every_seconds 必须是 10～86400 范围内的 10 秒倍数")
	}
	if c.ContainerRescanSeconds < 1 || c.ContainerRescanSeconds > 3600 || c.ContainerRetrySeconds < 1 || c.ContainerRetrySeconds > 3600 {
		return c, fmt.Errorf("容器日志重扫/重试秒数必须在 1～3600 之间")
	}
	if c.ContainerLineBytes < 1024 || c.ContainerLineBytes > 8192 {
		return c, fmt.Errorf("container_line_bytes 必须在 1024～8192 之间，给 JSON 转义和记录元信息留出空间")
	}
	if c.TransferProgressSeconds < 1 || c.TransferProgressSeconds > 3600 {
		return c, fmt.Errorf("transfer_progress_seconds 必须在 1～3600 之间")
	}
	if c.RecordingFlushSeconds < 1 || c.RecordingFlushSeconds > 60 {
		return c, fmt.Errorf("recording_flush_seconds 必须在 1～60 之间")
	}
	if c.RecentCapacityMiB < 16 || c.RecentCapacityMiB > 1024 || c.RecentHours < 1 || c.RecentHours > 24 {
		return c, fmt.Errorf("近期副本容量必须在 16～1024 MiB，保留时间在 1～24 小时")
	}
	if c.CopyImportantSeconds < 1 || c.CopyImportantSeconds > 300 || c.CopyOrdinarySeconds < 1 || c.CopyOrdinarySeconds > 300 {
		return c, fmt.Errorf("近期副本发送间隔必须在 1～300 秒")
	}
	c.RuntimeLevel = strings.ToUpper(c.RuntimeLevel)
	switch c.RuntimeLevel {
	case "DEBUG", "INFO", "WARN", "ERROR":
	default:
		return c, fmt.Errorf("runtime_level 必须是 DEBUG、INFO、WARN 或 ERROR")
	}
	return c, nil
}
