// 项目页：页头、标签页和宽屏的项目栏。
export default {
  sections: {
    overview: "概览",
    network: "网络",
    settings: "设置",
  },
  breadcrumb: "项目",
  // n：节点数，英文按单复数用。
  nodes: (_n: number, names: string) => `节点 ${names}`,
  noContainers: "暂无容器",
  rebuiltHint: "面板丢失或更换后根据节点缓存重建的项目，原创建时间、历史版本和部署记录已丢失。",
  rebuiltAt: (ago: string) => `从节点缓存重建于 ${ago}`,
  createdAt: (ago: string) => `创建于 ${ago}`,
  containerCount: (n: number) => `${n} 个容器`,
  groupCount: (n: number) => `${n} 个容器组`,
  statefulCount: (n: number) => `${n} 个有状态`,
  column: {
    allProjects: "全部项目",
    project: "项目",
    containers: "容器",
    draft: "草稿",
    // 读屏软件在容器名后面读出的状态和节点。
    stateOnNode: (state: string, node: string) => `（${state}，节点 ${node}）`,
  },
};
