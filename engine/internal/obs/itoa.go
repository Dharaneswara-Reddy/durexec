package obs

import "strconv"

// itoa is a one-line wrapper kept separate so metrics.go reads as metric
// definitions and nothing else.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }
