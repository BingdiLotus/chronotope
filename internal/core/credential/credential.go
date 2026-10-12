// Package credential 是凭据引用的领域类型（凭据服务设计 §层 0——
// CredentialRef 的 nominal brand；期 7 纪律：类型下沉 core）。
package credential

// Ref 是凭据引用名（POSIX 风格——dsh 同款 nominal brand：防与普通
// 字符串混用）。进 spec digest 与 run 快照的是引用名——值永不过。
type Ref string

// Validate 引用名的合法性（POSIX 环境变量名——字母数字下划线，不以
// 数字开头）。
func (r Ref) Validate() bool {
	if r == "" || len(r) > 128 {
		return false
	}
	for i, c := range r {
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			continue
		}
		if c >= '0' && c <= '9' && i > 0 {
			continue
		}
		return false
	}
	return true
}
