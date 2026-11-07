# 安装面板

开始前确认服务器满足 [系统要求](../../README.md#系统要求)。

在 Linux 服务器上以 root 执行：

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sudo sh
```

脚本会下载 nagi 并校验 SHA-256，以 systemd 服务运行面板；面板默认内嵌一个节点，单机即可使用，未安装 Docker 时会自动安装。完成后终端会输出面板地址、公钥指纹和初始化链接，在浏览器中打开链接即可设置管理员账号。面板使用自签证书，浏览器提示不安全时，核对公钥指纹后继续即可。

## 常用选项

追加在命令末尾，例如 `| sudo sh -s -- --port 28443`：

| 选项 | 说明 |
| --- | --- |
| `--port <端口>` | 指定面板端口，默认首次启动时随机选择 |
| `--no-agent` | 只运行面板，不内嵌节点，适合把面板单独放在一台机器上 |
| `--docker-mirror <名称>` | 自动安装 Docker 时使用的软件源镜像：`Aliyun` 或 `AzureChinaCloud`，适用于国内服务器 |
| `--download-url <地址>` | 从镜像或内网地址下载 nagi，适用于无法访问 GitHub 的环境；之后一键更新面板也从该地址下载 |
| `--version <版本>` | 安装指定版本，默认为最新版本 |

下一步：[添加节点](add-node.md)。
