// 项目页：页头、标签页和宽屏的项目栏。
export default {
  sections: {
    overview: "概览",
    network: "网络",
    settings: "设置",
  },
  tabs: {
    containers: "容器",
    network: "网络",
    settings: "设置",
  },
  breadcrumb: "项目",
  stateful: "有状态",
  nodes: (names: string[]) => `节点 ${names.join("、")}`,
  noContainers: "暂无容器",
  rebuiltHint: "面板丢失或更换后根据节点缓存重建的项目，原创建时间、历史版本和部署记录已丢失。",
  rebuiltAt: (ago: string) => `从节点缓存重建于 ${ago}`,
  createdAt: (ago: string) => `创建于 ${ago}`,
  containerCount: (n: number) => `${n} 个容器`,
  groupCount: (n: number) => `${n} 个容器组`,
  countSeparator: "，",
  column: {
    containers: "容器",
    nodeOffline: "节点离线",
    draft: "草稿",
    stateOnNode: (state: string) => `${state}，节点`,
  },
};
