-- 容器的显示名称：只用于界面显示，可以随时改；为空时显示容器名。容器名创建后不能改（见 docs/agent.md#容器）。
ALTER TABLE containers ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
