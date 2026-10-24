package update

import (
	"path/filepath"
	"testing"
)

// stubRolloutPaths 把健康标记、启动标记与失败版本记录指到临时目录，测试不污染真实用户目录。
func stubRolloutPaths(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	prevHealthy, prevFailed, prevStarting := healthyMarkPathFn, failedVersionPathFn, startupMarkPathFn
	healthyMarkPathFn = func() string { return filepath.Join(dir, "healthy") }
	failedVersionPathFn = func() string { return filepath.Join(dir, "update-failed") }
	startupMarkPathFn = func() string { return filepath.Join(dir, "update-starting") }
	t.Cleanup(func() {
		healthyMarkPathFn, failedVersionPathFn, startupMarkPathFn = prevHealthy, prevFailed, prevStarting
	})
}

func TestHealthyMarkRoundTrip(t *testing.T) {
	stubRolloutPaths(t)
	if HealthyMarked("0.5.0") {
		t.Fatal("初始不应有健康标记")
	}
	if err := MarkHealthy("0.5.0"); err != nil {
		t.Fatalf("写标记失败: %v", err)
	}
	if !HealthyMarked("0.5.0") {
		t.Fatal("应记录健康版本")
	}
	if HealthyMarked("0.4.12") {
		t.Fatal("版本不匹配不应算健康")
	}
	ClearHealthyMark()
	if HealthyMarked("0.5.0") {
		t.Fatal("清除后不应再有标记")
	}
}

func TestFailedVersionRoundTrip(t *testing.T) {
	stubRolloutPaths(t)
	if FailedVersion() != "" {
		t.Fatal("初始失败版本应为空")
	}
	MarkFailedVersion("0.5.1")
	if got := FailedVersion(); got != "0.5.1" {
		t.Fatalf("应记录失败版本，实际 %q", got)
	}
	ClearFailedVersion()
	if FailedVersion() != "" {
		t.Fatal("清除后失败版本应为空")
	}
}

// 健康标记写入时顺手清掉失败版本：同一个版本健康了就不该再被跳过。
func TestMarkHealthyClearsFailedVersion(t *testing.T) {
	stubRolloutPaths(t)
	MarkFailedVersion("0.5.1")
	if err := MarkHealthy("0.5.1"); err != nil {
		t.Fatalf("写标记失败: %v", err)
	}
	if got := FailedVersion(); got != "" {
		t.Fatalf("健康后失败版本应被清掉，实际 %q", got)
	}
}
