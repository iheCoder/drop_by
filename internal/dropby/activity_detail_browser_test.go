package dropby

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestActivityDetailShowsTechnicalRecordInOneClick(t *testing.T) {
	// 使用浏览器自身的 details 展开行为。页面也可以独立打开排查回归。
	var browser string
	for _, candidate := range []string{"chromium", "chromium-browser", "google-chrome", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"} {
		if path, err := exec.LookPath(candidate); err == nil {
			browser = path
			break
		}
	}
	if browser == "" {
		t.Skip("本机没有可用的 Chromium 或 Chrome")
	}
	page, err := filepath.Abs("testdata/activity-detail.html")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, browser, "--headless", "--disable-background-networking", "--user-data-dir="+t.TempDir(), "--dump-dom", page).Output()
	if err != nil || !strings.Contains(string(out), `<output id="result">通过</output>`) {
		t.Fatalf("行动详情展开检查失败：%v\n%s", err, out)
	}
}
