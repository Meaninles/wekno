package imoutput

import (
	"fmt"
	"time"
)

const RunWaitingUpdateInterval = 5 * time.Second

// FormatRunWaiting returns the single user-visible status for a ReAct or
// Claude SDK run before answer text is available. The elapsed value advances
// only at five-second boundaries so replace-style IM transports are not
// flooded with cosmetic updates.
func FormatRunWaiting(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	bucket := elapsed / RunWaitingUpdateInterval
	if bucket == 0 {
		return "正在处理，请稍候"
	}
	seconds := int64(bucket * RunWaitingUpdateInterval / time.Second)
	return fmt.Sprintf("正在处理，请稍候 · 已用 %d 秒", seconds)
}
