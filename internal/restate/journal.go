package restate

// journal 大小策略（契约规范 §7 第 5 条）：restate.Run 返回值只存摘要 + 引用；
// 大 payload（工具输出/read_file 内容 >4KB）外置 PG/MinIO，journal 存 ref。
// RunStep 封装统一执行该策略——重放性能与 Restate 条目限制的双保险。

// MaxInlineBytes 是 journal 内联载荷上限。
const MaxInlineBytes = 4 * 1024

// JournalRef 是外置大 payload 的引用。
type JournalRef struct {
	Kind string `json:"kind"` // pg | minio
	Ref  string `json:"ref"`  // 外置存储引用（由 worker 写入后回填）
	Size int    `json:"size"`
}

// Summarized 是「可内联摘要 + 可选外置引用」的组合产物。
type Summarized struct {
	Inline []byte      `json:"inline"`
	Ref    *JournalRef `json:"ref,omitempty"`
}

// Summarize 将 payload 按 MaxInlineBytes 拆分；超出部分生成外置引用（Ref 留空，
// 由调用方写入 PG/MinIO 后回填，保证 journal 条目大小有界）。
func Summarize(payload []byte, kind string) Summarized {
	if len(payload) <= MaxInlineBytes {
		return Summarized{Inline: payload}
	}
	return Summarized{
		Inline: payload[:MaxInlineBytes],
		Ref:    &JournalRef{Kind: kind, Size: len(payload)},
	}
}
