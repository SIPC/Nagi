// 由中文配图生成英文配图的源文件：<名字>.html → <名字>.en.html，只替换文字，布局和样式与中文版相同。
// 修改中文配图的文字后，在下面同步译文，再执行 node docs/assets/src/translate.mjs 和 render.mjs。
// 页面里还有未翻译的中文（注释除外）时报错，免得漏译。
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));

const t = {
  // 中文标语的字间距按汉字设，英文用小一些的。
  hero: [
    ["风平浪静", "Calm waters"],
    ["letter-spacing: 0.4em;", "letter-spacing: 0.08em;"],
  ],
  architecture: [
    ["一个面板，管理多个节点", "One panel, many nodes"],
    ["每个节点主动连接面板，无需开放端口；节点之间通过 WireGuard 直接通信。", "Nodes connect out to the panel, so no inbound ports are needed; nodes talk to each other directly over WireGuard."],
    ['<i class="dot"></i>面板</div>', '<i class="dot"></i>Panel</div>'],
    ["网页控制台", "Web console"],
    ["备用面板", "Standby panel"],
    ["随时接替", "Ready to take over"],
    ["节点 1", "Node 1"],
    ["节点 2", "Node 2"],
    ["节点 3", "Node 3"],
    ["运行容器", "Runs containers"],
    ["无需开放端口", "No inbound ports"],
    ["由节点主动连接面板，位于内网的服务器也可加入。", "Nodes dial the panel, so servers behind NAT can join too."],
    ["节点直接互通", "Direct node links"],
    ["节点间组成 WireGuard 网络，不通时由面板中转。", "Nodes form a WireGuard mesh, relayed by the panel when needed."],
    ["面板离线不影响服务", "Panel outages are harmless"],
    ["节点缓存配置，面板离线时容器继续运行。", "Nodes cache their config, so containers keep running."],
    ['label: "主动连接"', 'label: "dials"'],
    ['label: "同步"', 'label: "sync"'],
  ],
  model: [
    ["一个项目，一组容器", "A project is a group of containers"],
    ["前端、后端、数据库放进同一项目，每个容器各自选择节点；现有 compose 可直接导入为容器组。", "Put the frontend, backend and database in one project, each on its own node; import an existing compose file as a container group."],
    ["项目<span>", "Project<span>"],
    ['<div class="k">容器组 · ', '<div class="k">Container group · '],
    ['<div class="k">容器</div>', '<div class="k">Container</div>'],
    ["节点 1", "Node 1"],
    ["节点 2", "Node 2"],
    ["由镜像表单生成", "From the image form"],
    ["层级扁平", "Flat structure"],
    ["只有容器与容器组两种，容器组展开可见其中的容器。", "Just containers and container groups; expand a group to see its containers."],
    ["独立部署", "Deployed independently"],
    ["每个容器可单独部署、回滚和迁移，互不影响。", "Deploy, roll back and move each container on its own."],
    ["随时导出", "Export any time"],
    ["底层均为标准 compose，导出即可独立运行。", "Everything is plain compose underneath and runs without Nagi."],
  ],
  versions: [
    ["每次部署，都是一个版本", "Every deployment is a version"],
    ["修改先存为草稿，部署时生成新版本；可随时回滚到任一版本，恢复当时的状态。", "Changes are saved as a draft and become a new version on deploy; roll back to any version to restore it exactly."],
    [">回滚<", ">Roll back<"],
    ["首次部署", "First deploy"],
    ["更新镜像", "New image"],
    ["上传新代码", "New code"],
    ["回滚到 v2", "Back to v2"],
    ['<div class="vn">草稿</div>', '<div class="vn">Draft</div>'],
    ["未部署的修改", "Undeployed changes"],
    ["每个版本包含", "Each version includes"],
    ['<div class="t">配置</div>', '<div class="t">Config</div>'],
    ["compose、环境变量与密钥", "Compose, environment variables and secrets"],
    ['<div class="t">文件</div>', '<div class="t">Files</div>'],
    ["部署时的代码与静态文件", "Code and static files as deployed"],
    ['<div class="t">镜像</div>', '<div class="t">Images</div>'],
    ["按摘要记录，即使使用 latest 也能精确还原", "Pinned by digest, so even latest restores exactly"],
  ],
  exposure: [
    ["统一入口，自动 HTTPS", "One entry point, automatic HTTPS"],
    ["容器不直接映射端口，所有对外访问都经节点上内嵌的 Caddy 转发。", "Containers don't publish ports; all external traffic goes through the Caddy built into each node."],
    ["网站 · HTTPS", "Website · HTTPS"],
    ["数据库 · TCP", "Database · TCP"],
    ["游戏服务器 · UDP", "Game server · UDP"],
    ["节点上的统一入口", "Entry point on the node"],
    ["域名与 HTTPS", "Domains and HTTPS"],
    ["TCP / UDP 转发", "TCP / UDP forwarding"],
    ["访问量与耗时统计", "Request stats"],
    ['<div class="k">容器</div>', '<div class="k">Container</div>'],
    ["自动证书", "Automatic certificates"],
    ["绑定域名后自动申请和续期证书；签发前仍可通过 HTTP 访问。", "Certificates are issued and renewed automatically; HTTP works until they are ready."],
    ["即时生效", "Instant changes"],
    ["修改端口和域名立即生效，无需重新部署容器。", "Port and domain changes apply at once, without redeploying."],
    ["出口唯一", "Single entry"],
    ["所有流量经同一入口，不会绕过防火墙规则。", "All traffic passes one entry point and can't bypass firewall rules."],
  ],
  join: [
    ["一行命令，加入新节点", "Join a node with one command"],
    ["在面板上复制安装命令，在新服务器上执行，节点即自动加入面板。", "Copy the install command from the panel and run it on the new server; the node joins on its own."],
    ["复制安装命令", "Copy the command"],
    ["面板 · 添加节点", "Panel · Add node"],
    [">复制<", ">Copy<"],
    ["在新服务器上执行", "Run it on the server"],
    ["新服务器", "New server"],
    ["检查 Docker 与 compose", "Check Docker and compose"],
    ["下载并校验 nagi", "Download and verify nagi"],
    ["加入面板", "Join the panel"],
    ["安装为系统服务", "Install as a service"],
    ["节点上线", "Node online"],
    ["面板 · 节点", "Panel · Nodes"],
    ["节点 1", "Node 1"],
    ["节点 2", "Node 2"],
    ["节点 3", "Node 3"],
    [">在线<", ">Online<"],
    ["刚刚加入", "Just joined"],
    ["一次性令牌", "One-time token"],
    ["令牌仅可使用一次，1 小时内有效。", "Works once and expires in 1 hour."],
    ["身份校验", "Verified identity"],
    ["全程校验面板公钥，防止他人冒充面板。", "The panel's public key is checked throughout, so nothing can impersonate it."],
    ["版本一致", "Matching version"],
    ["自动下载与面板相同的版本，并校验 SHA-256。", "Downloads the panel's version and verifies its SHA-256."],
  ],
  intranet: [
    ["跨节点，按名字访问", "Reach containers by name across nodes"],
    ["同一项目的容器即使分布在不同节点上，也可直接通过容器名互相访问，无需额外配置网络。", "Containers in a project reach each other by name, even on different nodes, with no network setup."],
    ['<div class="zl">节点 1</div>', '<div class="zl">Node 1</div>'],
    ['<div class="zl">节点 2</div>', '<div class="zl">Node 2</div>'],
    ['<div class="k">容器</div>', '<div class="k">Container</div>'],
    ['<div class="k">代理</div>', '<div class="k">Proxy</div>'],
    ["指向节点 2", "To node 2"],
    ["按名称访问", "Access by name"],
    ["使用容器名即可访问，与容器所在的节点无关。", "Use the container name wherever the container runs."],
    ["迁移后不变", "Stable across moves"],
    ["容器迁移到其他节点后，名称和访问方式不变。", "Names and addresses stay the same after a move."],
    ["默认隔离", "Isolated by default"],
    ["不同项目之间默认隔离，可按需连接两个项目。", "Projects are isolated; link two projects when needed."],
  ],
  recovery: [
    ["面板故障，服务不受影响", "Panel down, services stay up"],
    ["配置保存在面板，容器与数据位于节点；每个节点另外缓存一份本节点的配置。", "Config lives on the panel, containers and data on the nodes; each node also caches its own config."],
    ['<i class="dot"></i>面板</div>', '<i class="dot"></i>Panel</div>'],
    ["项目配置 · 版本记录 · 部署记录", "Projects · Versions · Deployments"],
    ["节点 1", "Node 1"],
    ["节点 2", "Node 2"],
    ["容器 · 数据 · 快照", "Containers · Data · Snapshots"],
    ["配置缓存", "Config cache"],
    ["面板暂时离线", "Panel briefly offline"],
    ["容器与快照照常运行，恢复后即可继续管理。", "Containers and snapshots carry on; manage them again once it is back."],
    ["已配置备用面板", "With a standby panel"],
    ["在备用面板上一键提升为主面板，数据完整。", "Promote the standby to primary with one click, data intact."],
    ["面板彻底丢失", "Panel lost for good"],
    ["安装新面板后，项目根据各节点的配置缓存自动重建。", "Install a new panel and projects are rebuilt from the nodes' caches."],
    ['"下发配置"', '"pushes config"'],
  ],
  failover: [
    ["节点故障，自动接替", "Automatic failover when a node goes down"],
    ["为容器指定一个备用节点。原节点、备用节点和见证人各有一票，获得两票的一方才能运行容器。", "Give a container a standby node. The original node, the standby and a witness each have one vote; only the side with two votes runs it."],
    ['<div class="k">原节点</div>', '<div class="k">Original node</div>'],
    ["节点 1", "Node 1"],
    ["已失联，0 票", "Unreachable, 0 votes"],
    ['<div class="k">备用节点</div>', '<div class="k">Standby node</div>'],
    ["节点 2", "Node 2"],
    ["自己 + 见证人 = 2 票", "Self + witness = 2 votes"],
    ['<div class="k">见证人</div>', '<div class="k">Witness</div>'],
    ['<i class="dot idle"></i>面板</div>', '<i class="dot idle"></i>Panel</div>'],
    ["只投票，不运行容器", "Votes only, runs nothing"],
    ["</span>平时</div>", "</span>Normally</div>"],
    ["原节点每&nbsp;10&nbsp;秒向另外两方续约，保持两票，容器持续运行。", "The original node renews with the other two every&nbsp;10&nbsp;s, keeping two votes and running the container."],
    ["</span>失联</div>", "</span>Cut off</div>"],
    ["原节点续约失败，会在租约（", "When renewals fail, it stops the container itself before the lease ("],
    ["30 秒", "30 s"],
    ["）到期前自行停止容器。", ") expires."],
    ["</span>接替</div>", "</span>Takeover</div>"],
    ["租约到期后，见证人改投备用节点；备用节点获得两票后启动容器。", "Once the lease expires, the witness votes for the standby, which starts the container with two votes."],
    ["三票中只可能有一方获得两票，因此容器不会在两个节点上同时运行。", "Only one side can hold two of the three votes, so a container never runs on two nodes at once."],
    ["<b>备用节点故障</b>不受影响", "<b>Standby fails</b> no impact"],
    ["<b>面板离线</b>不受影响", "<b>Panel offline</b> no impact"],
    ["<b>原节点恢复</b>稳定后自动切回", "<b>Original returns</b> moves back once stable"],
    ['label: "投票"', 'label: "vote"'],
  ],
};

const han = /[\u4e00-\u9fff]/;
let failed = false;
for (const [name, pairs] of Object.entries(t)) {
  let html = readFileSync(join(here, `${name}.html`), "utf8").replace(/\r\n/g, "\n");
  html = html.replace('<html lang="zh-CN">', '<html lang="en">');
  // 长的先替换，免得短词（如「配置缓存」）先把长句拆碎。
  for (const [zh, en] of [...pairs].sort((a, b) => b[0].length - a[0].length)) {
    if (!html.includes(zh)) {
      console.error(`${name}: 找不到「${zh}」`);
      failed = true;
      continue;
    }
    html = html.replaceAll(zh, en);
  }
  // 注释里的中文不算。
  const left = html
    .split("\n")
    .filter((l) => han.test(l.replace(/\/\/.*$/, "").replace(/<!--.*?-->/g, "").replace(/\/\*.*?\*\//g, "")));
  if (left.length) {
    console.error(`${name}: 还有未翻译的中文：\n  ${left.map((l) => l.trim()).join("\n  ")}`);
    failed = true;
  }
  writeFileSync(join(here, `${name}.en.html`), html);
}
if (failed) process.exit(1);
console.log("ok");
