# 添加备用面板

在主面板「设置 → 备用面板」中生成配对码，在新服务器上执行：

```bash
curl -fsSL https://github.com/SIPC/Nagi/releases/latest/download/install.sh | sudo sh -s -- --standby <配对码>
```

备用面板实时同步主面板的数据，平时只读。主面板故障时，可在备用面板上将其提升为主面板。
