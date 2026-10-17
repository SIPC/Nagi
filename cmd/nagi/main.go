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
	"github.com/SIPC/nagi/internal/pkg/invite"
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
	root.AddCommand(serverCmd(), agentCmd(), enrollCmd(), panelCmd(), versionCmd())
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
		Version:      version,
		DisableProxy: noProxy,
		Logger:       log.With("component", "agent"),
	}
	// 只在第一次启动时自动加入本面板。之后即使本机节点被移交给了别的面板，也不会再自动加回来。
	if !agent.HasPanels(agentDir) {
		token, err := srv.CreateEnrollToken(ctx)
		if err != nil {
			return err
		}
		cfg.Panel, cfg.Fingerprint, cfg.Token = srv.LocalAddr(), srv.Fingerprint(), token
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

// panelCmd 是节点本地命令：查看和修改信任的面板列表。用于旧面板已经挂掉、无法再发指令的情况。
// 需要 root（能读写节点数据目录）。运行中的节点会在 10 秒内按新列表调整连接。
func panelCmd() *cobra.Command {
	var dataDir string
	cmd := &cobra.Command{
		Use:   "panel",
		Short: "查看和修改节点信任的面板（面板自带的节点用 --data-dir <面板数据目录>/agent）",
	}
	cmd.PersistentFlags().StringVar(&dataDir, "data-dir", "/var/lib/nagi", "节点数据目录")

	list := &cobra.Command{
		Use:   "list",
		Short: "列出信任的面板",
		RunE: func(cmd *cobra.Command, _ []string) error {
			panels, err := agent.ListPanels(dataDir)
			if err != nil {
				return err
			}
			if len(panels) == 0 {
				fmt.Println("没有信任任何面板")
			}
			for _, p := range panels {
				fmt.Println(p.Address, p.Fingerprint)
			}
			return nil
		},
	}

	var server, fingerprint, token string
	add := &cobra.Command{
		Use:   "add [接入码]",
		Short: "加入一个面板：粘贴新面板生成的接入码，或者用 --server、--fingerprint、--token",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				inv, err := invite.Decode(args[0])
				if err != nil {
					return err
				}
				server, fingerprint, token = inv.Address, inv.Fingerprint, inv.Token
			}
			if server == "" || fingerprint == "" || token == "" {
				return errors.New("需要接入码，或者 --server、--fingerprint 和 --token")
			}
			ctx, cancel := signalContext()
			defer cancel()
			if err := agent.AddPanel(ctx, dataDir, server, fingerprint, token); err != nil {
				return err
			}
			fmt.Println("已加入面板", server)
			return nil
		},
	}
	add.Flags().StringVar(&server, "server", "", "面板地址，例如 203.0.113.10:34567")
	add.Flags().StringVar(&fingerprint, "fingerprint", "", "面板公钥指纹")
	add.Flags().StringVar(&token, "token", "", "加入令牌")

	var force bool
	remove := &cobra.Command{
		Use:   "remove <地址或指纹>",
		Short: "从信任列表移除一个面板",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := agent.RemovePanel(dataDir, args[0], force)
			if err != nil {
				return err
			}
			fmt.Println("已移除面板", p.Address)
			return nil
		},
	}
	remove.Flags().BoolVar(&force, "force", false, "允许移除最后一个面板")

	cmd.AddCommand(list, add, remove)
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
