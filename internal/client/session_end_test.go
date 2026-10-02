package client

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// Windows 的超时文案里没有 "timeout" 这个词，sessionEndInfo 原本认不出它，
// 于是「会话结束」那行会同时出现空的 reason 和一大串英文 err。
func TestSessionEndInfoCoversWindowsTimeout(t *testing.T) {
	winErr := errors.New("读隧道: read tcp 192.168.5.15:33983->103.142.86.145:62233: wsarecv: " +
		"A connection attempt failed because the connected party did not properly respond after " +
		"a period of time, or established connection failed because connected host has failed to respond.")

	lvl, reason := sessionEndInfo(winErr)
	if reason == "" {
		t.Fatal("Windows 超时文案必须能归出中文原因，否则日志里会同时出现空 reason 和英文 err")
	}
	if !strings.Contains(reason, "没有响应") {
		t.Errorf("reason = %q，期望包含「没有响应」", reason)
	}
	if lvl != slog.LevelWarn {
		t.Errorf("级别 = %v，期望 Warn", lvl)
	}
}

// 认不出来的错误保持空原因：会话结束那行会退回附原文，排障仍有线索。
func TestSessionEndInfoUnknownStaysEmpty(t *testing.T) {
	if _, reason := sessionEndInfo(errors.New("something entirely unexpected")); reason != "" {
		t.Errorf("认不出的错误不该编原因，实际 %q", reason)
	}
}
