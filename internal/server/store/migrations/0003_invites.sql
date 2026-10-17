-- 接入码：1 小时内可以给多个节点用的加入令牌。
ALTER TABLE enroll_tokens ADD COLUMN multi_use INTEGER NOT NULL DEFAULT 0;
ALTER TABLE enroll_tokens ADD COLUMN uses INTEGER NOT NULL DEFAULT 0;
