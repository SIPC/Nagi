-- 界面语言（"zh-CN"、"en"），空表示没有选过，跟随浏览器。
ALTER TABLE users ADD COLUMN language TEXT NOT NULL DEFAULT '';
