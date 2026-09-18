package xutil

// ToPtr 取值的地址。用于需要区分「没设置」和「设置成零值」的少数场景。
//
// 配置结构体不该用它：默认值预填进结构体就有同样的语义，见 internal/config。
func ToPtr[T any](v T) *T { return &v }

// GetOrDefault v 为零值时返回 defaultV
func GetOrDefault[T comparable](v, defaultV T) T {
	var zero T
	if v == zero {
		return defaultV
	}
	return v
}
