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
// 修改后重启生效；不包含业务库、旧日志库和 Docker 自己的轮换文件。
type Config struct {
	CapacityGiB         int    `json:"capacity_gib"`
	MetricsEverySeconds int    `json:"metrics_every_seconds"`
	RuntimeLevel        string `json:"runtime_level"`
}

func DefaultConfig() Config {
	return Config{CapacityGiB: 5, MetricsEverySeconds: 10, RuntimeLevel: "INFO"}
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
	c.RuntimeLevel = strings.ToUpper(c.RuntimeLevel)
	switch c.RuntimeLevel {
	case "DEBUG", "INFO", "WARN", "ERROR":
	default:
		return c, fmt.Errorf("runtime_level 必须是 DEBUG、INFO、WARN 或 ERROR")
	}
	return c, nil
}
