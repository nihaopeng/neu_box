package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Worker 的错误响应要映射成非 0 退出码与可读的 stderr 摘要。
func TestWorkerErrorReturnsNonZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(t, writer, http.StatusConflict, map[string]any{
			"error": "already allocated",
			"code":  "docker_container_pid_changed",
		})
	}))
	defer server.Close()

	application, _, errOut := testApplication(server.URL)
	code := application.run([]string{"acquire", "--device-num", "1"})
	if code != 1 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(errOut.String(), "http_status:") || !strings.Contains(errOut.String(), "409") {
		t.Fatalf("unexpected stderr: %s", errOut.String())
	}
}

func TestHelpUsesReadableIndentation(t *testing.T) {
	application, out, errOut := testApplication("http://127.0.0.1:1")
	code := application.run([]string{"--help"})
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	for _, expected := range []string{
		"\n    neubox shell",
		"\n    neubox acquire",
		"neubox submit --wait --device-num 2 -- neubox docker run --rm",
		"neubox submit --wait --device-num 2 -- neubox docker start train-1 -a",
		"neubox submit --wait --device-num 2 --script - <<'SH'\nset -e\n",
		"\ndocker exec dev python /workspace/train.py\nSH\n",
		"\n    neubox docker restart",
		"\n    neubox docker status",
		"neubox help verbose",
	} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("missing %q in help:\n%s", expected, out.String())
		}
	}
	if strings.Contains(out.String(), "annotation") || strings.Contains(out.String(), "NEU_BOX_URL") {
		t.Fatalf("默认帮助应仅保留常用命令: %s", out.String())
	}
	out.Reset()
	if code := application.run([]string{"help", "verbose"}); code != 0 {
		t.Fatalf("verbose exit=%d", code)
	}
	for _, expected := range []string{"priority 1", "NEU_BOX_URL", "任务不会自动接管运行中容器", "完整示例: neubox help"} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("详细帮助缺少 %q: %s", expected, out.String())
		}
	}
}

func TestDockerHelpUsesUnifiedHelp(t *testing.T) {
	application, out, errOut := testApplication("http://127.0.0.1:1")
	if code := application.run([]string{"help"}); code != 0 {
		t.Fatalf("help exit=%d", code)
	}
	mainHelp := out.String()
	for _, args := range [][]string{{"docker", "help"}, {"help", "docker"}, {"docker", "--help"}} {
		out.Reset()
		if code := application.run(args); code != 0 || errOut.Len() != 0 {
			t.Fatalf("%v: exit=%d stderr=%s", args, code, errOut.String())
		}
		if out.String() != mainHelp {
			t.Fatalf("%v: Docker help 与主 help 不一致", args)
		}
	}
	out.Reset()
	if code := application.run([]string{"help", "verbose"}); code != 0 {
		t.Fatalf("verbose exit=%d", code)
	}
	verboseHelp := out.String()
	for _, args := range [][]string{{"docker", "help", "verbose"}, {"help", "docker", "verbose"}} {
		out.Reset()
		if code := application.run(args); code != 0 || out.String() != verboseHelp {
			t.Fatalf("%v: Docker verbose 与主 verbose 不一致", args)
		}
	}
}

func TestPrintJSONDecodesEscapedUnicode(t *testing.T) {
	output := &bytes.Buffer{}
	if err := printJSON(output, []byte(`{"message":"\u5df2\u91ca\u653e"}`)); err != nil {
		t.Fatal(err)
	}
	if output.String() != "{\n  \"message\": \"已释放\"\n}\n" {
		t.Fatalf("unexpected output: %q", output.String())
	}
}
