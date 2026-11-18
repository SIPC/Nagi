// 服务页：容器组里一个服务的概览、日志、终端和监控。
export default {
  tabs: { overview: "概览", logs: "日志", terminal: "终端", metrics: "监控" },
  belongsTo: "属于容器组",
  notFound: (service: string, group: string) => `容器组 ${group} 中不存在服务 ${service}，可能已被删除。`,
  backToGroup: "返回容器组",
  image: "镜像",
  health: "健康检查",
  healthLabels: { healthy: "健康", unhealthy: "不健康", starting: "检查中" } as Record<string, string>,
  restarts: "重启次数",
  replicas: "副本",
  ports: "端口映射",
  resources: "资源",
  restartConfirm: (name: string) => `重启服务 ${name}？该服务短暂中断，容器组的其他服务不受影响。`,
  stopConfirm: (name: string) => `停止服务 ${name}？该服务将不可访问，容器组的其他服务继续运行。`,
};
