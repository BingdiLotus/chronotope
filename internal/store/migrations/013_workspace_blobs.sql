-- 工作区 blob 合同（期 2 §A）：内容寻址对象图 + 目录索引。
-- workspace_files 是「空间面」的细粒度真相：path → sha256 内容寻址（对象存
-- RustFS bucket workspaces/{org}/{session}/blobs/{hash}）+ 目录索引。
-- 与快照卷 tar（粗粒度秒级恢复）互补：blob 合同是增量同步与「销毁后无快照
-- 重建」的第二条恢复链（fork 空间面懒恢复的基础）。
CREATE TABLE IF NOT EXISTS workspace_files (
  session_id text NOT NULL REFERENCES sessions (id),
  path       text NOT NULL,
  hash       text NOT NULL,
  size       bigint NOT NULL DEFAULT 0,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, path)
);
