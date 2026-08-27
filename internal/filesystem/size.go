package filesystem

import "fmt"

// HumanSize formats a byte count using binary (1024-based) units, matching
// what `ls -lh` and `du -h` report. Decimal (1000-based) units are what
// disk manufacturers use; we pick binary because developers comparing
// against other CLI tools expect it.
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 5 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
