# 添加节点

在面板的「节点」页复制安装命令，在新服务器上以 root 执行。脚本从 GitHub Release 下载与面板相同版本的 nagi 并校验 SHA-256，节点同样会自动安装 Docker。

无法访问 GitHub 时，在命令末尾加上 `--download-url <镜像地址>`，之后在面板上一键升级节点也会从该地址下载。

![一行命令，加入新节点](../assets/join.png)
