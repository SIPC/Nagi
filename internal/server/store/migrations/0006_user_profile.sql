-- 个人资料：显示名称（空表示显示用户名）和头像（图片文件的内容，空表示没有）。
ALTER TABLE users ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN avatar BLOB NOT NULL DEFAULT x'';
