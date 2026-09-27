package client

import "fmt"

// formatBytes 把字节数变成一眼能读懂的短串。
//
// 不固定用 MB 显示：刚连上时往往是几百 KB，统一按 MB 会全变成 0.00 MB，
// 看着像没在工作。
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(n)
	i := -1
	for value >= unit && i < len(units)-1 {
		value /= unit
		i++
	}
	return fmt.Sprintf("%.2f %s", value, units[i])
}
