# 升级与卸载

## 升级

在面板的「设置 → 版本更新」一键更新面板，也可以再次执行安装脚本，端口、数据目录等配置保持不变；节点可在面板的「节点」页一键升级。

## 卸载

面板和节点通用：

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/uninstall.sh | sudo sh
```

默认只卸载程序和服务，项目容器与 Nagi 数据目录保留；加 `-s -- --purge` 会同时删除 Nagi 管理的容器、网络和数据目录。项目文件（容器目录，默认在 `/srv/nagi` 下）不会被删除，脚本执行完会列出它们的位置。Docker 本身不卸载。

卸载节点前，建议先在面板上迁移或删除它的容器。
