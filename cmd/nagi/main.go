// nagi 是 Nagi 的唯一二进制，用子命令区分角色。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/SIPC/nagi/internal/agent"
	"github.com/SIPC/nagi/internal/server"
	"github.com/spf13/cobra"
)

// version 在发布构建时通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	root := &cobra.Command{
		Use:           "nagi",
		Short:         "Nagi（凪）：风平浪静的 Docker 项目面板",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(serverCmd(), agentCmd(), enrollCmd(), versionCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		os.Exit(1)
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func serverCmd() *cobra.Command {
	var (
		dataDir string
		port    int
		noAgent bool
		noProxy bool
	)
	cmd := &cobra.Command{
		Use:   "server",
		Short: "启动面板（默认同时启动一个内嵌节点）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))

			srv, err := server.New(ctx, server.Config{
				DataDir: filepath.Join(dataDir, "panel"),
				Port:    port,
				Logger:  log.With("component", "panel"),
			})
			if err != nil {
				return err
			}

			errc := make(chan error, 2)
			go func() { errc <- srv.Run(ctx) }()

			if !noAgent {
				go func() { errc <- runEmbeddedAgent(ctx, srv, dataDir, noProxy, log) }()
			}

			err = <-errc
			cancel()
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "/var/lib/nagi", "数据目录")
	cmd.Flags().IntVar(&port, "port", 0, "面板端口（默认首次启动时随机选择并记住）")
	cmd.Flags().BoolVar(&noAgent, "no-agent", false, "只运行面板，不启动内嵌节点")
	cmd.Flags().BoolVar(&noProxy, "no-proxy", false, "内嵌节点不启动 Caddy（仅用于开发）")
	return cmd
}

// runEmbeddedAgent 启动面板自带的节点。它和远程节点走完全相同的加入流程和控制连接。
func runEmbeddedAgent(ctx context.Context, srv *server.Server, dataDir string, noProxy bool, log *slog.Logger) error {
	agentDir := filepath.Join(dataDir, "agent")
	cfg := agent.Config{
		DataDir:      agentDir,
		Panel:        srv.LocalAddr(),
		Fingerprint:  srv.Fingerprint(),
		Version:      version,
		DisableProxy: noProxy,
		Logger:       log.With("component", "agent"),
	}
	if !agent.Enrolled(agentDir, srv.Fingerprint()) {
		token, err := srv.CreateEnrollToken(ctx)
		if err != nil {
			return err
		}
		cfg.Token = token
	}
	return agent.Run(ctx, cfg)
}

func agentCmd() *cobra.Command {
	var cfg agent.Config
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "启动节点，主动连接面板",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			cfg.Version = version
			cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
			return agent.Run(ctx, cfg)
		},
	}
	cmd.Flags().StringVar(&cfg.DataDir, "data-dir", "/var/lib/nagi", "数据目录")
	cmd.Flags().StringVar(&cfg.Panel, "server", "", "面板地址，例如 203.0.113.10:34567（首次加入时需要）")
	cmd.Flags().StringVar(&cfg.Fingerprint, "fingerprint", "", "面板公钥指纹（首次加入时需要）")
	cmd.Flags().StringVar(&cfg.Token, "token", "", "一次性加入令牌（首次加入时需要）")
	cmd.Flags().BoolVar(&cfg.DisableProxy, "no-proxy", false, "不启动 Caddy（仅用于开发）")
	return cmd
}

func enrollCmd() *cobra.Command {
	var cfg agent.Config
	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "用一次性令牌加入面板（安装脚本会调用它）",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext()
			defer cancel()
			return agent.Enroll(ctx, cfg)
		},
	}
	cmd.Flags().StringVar(&cfg.DataDir, "data-dir", "/var/lib/nagi", "数据目录")
	cmd.Flags().StringVar(&cfg.Panel, "server", "", "面板地址，例如 203.0.113.10:34567")
	cmd.Flags().StringVar(&cfg.Fingerprint, "fingerprint", "", "面板公钥指纹")
	cmd.Flags().StringVar(&cfg.Token, "token", "", "一次性加入令牌")
	return cmd
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "显示版本",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Println(version)
		},
	}
}
