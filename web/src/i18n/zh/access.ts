// 设置页「面板访问」：对外地址、绑定域名、公钥指纹。
export default {
  address: {
    label: "对外地址",
    usage: "节点和备用面板用它连接本面板",
    unset: "未设置",
    help: "节点通过「对外地址:面板端口」连接面板，安装命令同样使用该地址，须可从所有节点访问。修改后，在线节点接收新地址并在下次重连时使用；修改时离线的节点须在节点上执行 nagi panel add 更新。",
  },
  domain: {
    label: "绑定域名",
    bound: (domain: string) => `通过 https://${domain} 访问面板`,
    unbound: "未绑定，通过 https://<对外地址>:<端口> 访问",
    help: "域名须预先解析到本机。绑定后由本机 Caddy 经 80/443 端口反向代理面板并自动申请证书；节点仍通过对外地址和面板端口连接。",
    bind: "绑定",
    unbind: "解除绑定",
    unbindConfirm: (domain: string) => `解除绑定 ${domain}？面板不再通过此域名访问，正在通过此域名打开的页面会断开。`,
  },
  fingerprint: {
    label: "公钥指纹",
    help: "节点和备用面板连接时核对此指纹，以确认连接的是本面板。",
  },
};
