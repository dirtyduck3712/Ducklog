// Package model 定義日誌的核心型別。
package model

import "time"

// Level 對應 spec 的 level 編碼:0=debug 1=info 2=warn 3=error。
type Level uint8

const (
	Debug Level = iota
	Info
	Warn
	Error
)

var levelNames = [...]string{"debug", "info", "warn", "error"}

func (l Level) String() string {
	if int(l) < len(levelNames) {
		return levelNames[l]
	}
	return "unknown"
}

// ParseLevel 把字串 level 轉成 Level;第二個回傳值為是否有效。
func ParseLevel(s string) (Level, bool) {
	for i, n := range levelNames {
		if n == s {
			return Level(i), true
		}
	}
	return 0, false
}

// LogEntry 是一筆待寫入的日誌。TraceID 為空字串代表 NULL。
// Attrs 是原始 JSON 字串(預設 "{}")。ClockSkewed 由 ingest 在覆蓋 ts 時設為 true。
type LogEntry struct {
	TS          time.Time
	IngestedAt  time.Time
	Service     string
	Level       Level
	TraceID     string
	Message     string
	Attrs       string
	ClockSkewed bool
}
