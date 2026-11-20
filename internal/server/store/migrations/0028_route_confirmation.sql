-- 从节点缓存恢复的跨节点入口须先在面板确认。
ALTER TABLE routes ADD COLUMN pending_confirmation INTEGER NOT NULL DEFAULT 0;
