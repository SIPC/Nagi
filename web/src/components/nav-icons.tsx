// 主导航的图标：导航和命令面板共用，同一个入口在两处是同一个图标。和安卓 App 底部胶囊导航的图标一样
// （android/core/ui/.../Icons.kt）：概览 House、项目 Folder、节点 Server、设置 Settings。审计 App 上没有对应的入口。
import { Folder, House, ScrollText, Server, Settings } from "lucide-react";

export const navIcons = {
  overview: House,
  projects: Folder,
  nodes: Server,
  audit: ScrollText,
  settings: Settings,
};
