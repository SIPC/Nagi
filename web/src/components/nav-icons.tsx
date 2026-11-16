// 主导航的图标：导航和命令面板共用，同一个入口在两处是同一个图标。
import { Box, ScrollText, Server, SlidersHorizontal } from "lucide-react";

// OverviewIcon：概览。一条起伏的折线，和 lucide 的 16 号图标同样的线宽。
export function OverviewIcon() {
  return (
    <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth={1.5} strokeLinecap="round" strokeLinejoin="round" aria-hidden>
      <path d="M1.5 8h3l2-5 3 10 2-5h3" />
    </svg>
  );
}

export const navIcons = {
  overview: OverviewIcon,
  projects: Box,
  nodes: Server,
  audit: ScrollText,
  settings: SlidersHorizontal,
};
