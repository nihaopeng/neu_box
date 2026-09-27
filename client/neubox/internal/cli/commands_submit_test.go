package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubmitBuildsDockerTarget(t *testing.T) {
	var received commandRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/tasks" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
		}
		decodeRequest(t, request, &received)
		writeJSON(t, writer, http.StatusAccepted, map[string]any{
			"task_id":  "abc123",
			"position": 2,
		})
	}))
	defer server.Close()

	application, out, errOut := testApplication(server.URL)
	code := application.run([]string{
		"submit",
		"--device", "1",
		"--device", "3",
		"--cpu", "4",
		"--mem", "8",
		"--image", "training-01",
		"--workdir", "/workspace",
		"--container-user", "root",
		"--env", "MODE=perf",
		"--", "python", "train.py",
	})

	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if received.DeviceNum != 0 || len(received.DeviceIDs) != 2 || received.Target == nil {
		t.Fatalf("unexpected request: %+v", received)
	}
	if received.Command != "python train.py" {
		t.Fatalf("unexpected command: %q", received.Command)
	}
	if received.Target.Type != "docker" || received.Target.Image != "training-01" || received.Target.Env["MODE"] != "perf" {
		t.Fatalf("unexpected target: %+v", received.Target)
	}
	if received.Target.Workdir == nil || *received.Target.Workdir != "/workspace" {
		t.Fatalf("unexpected workdir: %+v", received.Target.Workdir)
	}
	if !strings.Contains(out.String(), "ID=abc123") {
		t.Fatalf("unexpected output: %s", out.String())
	}
}

func TestSubmitHostCarriesCurrentDirectoryAndSelectedEnvironment(t *testing.T) {
	var received commandRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		decodeRequest(t, request, &received)
		writeJSON(t, writer, http.StatusAccepted, map[string]any{"task_id": "host1", "position": 1})
	}))
	defer server.Close()

	application, _, errOut := testApplication(server.URL)
	project := t.TempDir()
	application.getwd = func() (string, error) { return project, nil }
	t.Setenv("NEUBOX_TEST_RUN", "abc")
	if code := application.run([]string{"submit", "--env", "NEUBOX_TEST_RUN", "--", "python", "train.py"}); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if received.Target == nil || received.Target.Type != "host" || received.Target.Workdir == nil ||
		*received.Target.Workdir != project || received.Target.Env["NEUBOX_TEST_RUN"] != "abc" {
		t.Fatalf("host context was lost: %+v", received.Target)
	}
}

func TestSubmitDockerProjectAndOutputPersistOnHost(t *testing.T) {
	var received commandRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		decodeRequest(t, request, &received)
		writeJSON(t, writer, http.StatusAccepted, map[string]any{"task_id": "docker1", "position": 1})
	}))
	defer server.Close()

	application, _, errOut := testApplication(server.URL)
	project := t.TempDir()
	output := filepath.Join(project, "runs", "exp1")
	application.getwd = func() (string, error) { return project, nil }
	if code := application.run([]string{
		"submit", "--image", "training:latest", "--project", "--output", output,
		"--", "python", "train.py",
	}); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if info, err := os.Stat(output); err != nil || !info.IsDir() {
		t.Fatalf("output directory was not created: %v", err)
	}
	if received.Target == nil || received.Target.Workdir == nil || *received.Target.Workdir != "/workspace" {
		t.Fatalf("project workdir missing: %+v", received.Target)
	}
	if len(received.Target.Mounts) != 2 ||
		received.Target.Mounts[0] != (commandMount{Source: project, Target: "/workspace"}) ||
		received.Target.Mounts[1] != (commandMount{Source: output, Target: "/outputs"}) {
		t.Fatalf("project/output mounts missing: %+v", received.Target.Mounts)
	}
}

func TestSubmitWaitStreamsTheSubmittedTask(t *testing.T) {
	var requestedResult bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/tasks":
			writeJSON(t, writer, http.StatusAccepted, map[string]any{"task_id": "task-wait", "position": 1})
		case "/tasks/task-wait":
			requestedResult = true
			writeJSON(t, writer, http.StatusOK, map[string]any{
				"task_id": "task-wait", "status": "completed", "result": map[string]any{"returncode": 0},
			})
		case "/tasks/task-wait/log":
			writeJSON(t, writer, http.StatusOK, map[string]any{
				"data": "training done\n", "offset": 0, "total_size": 14,
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	application, out, errOut := testApplication(server.URL)
	if code := application.run([]string{"submit", "--wait", "--", "python", "train.py"}); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !requestedResult || !strings.Contains(out.String(), "training done") {
		t.Fatalf("submit did not follow task: %s", out.String())
	}
}

func TestSubmitDefaultsToOneDevice(t *testing.T) {
	var received commandRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/tasks" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
		}
		decodeRequest(t, request, &received)
		writeJSON(t, writer, http.StatusAccepted, map[string]any{
			"task_id":  "abc123",
			"position": 1,
		})
	}))
	defer server.Close()

	application, _, errOut := testApplication(server.URL)
	code := application.run([]string{"submit", "--", "echo", "hi"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if received.DeviceNum != 1 {
		t.Fatalf("expected default device_num=1, got %+v", received)
	}
}

func TestExplicitZeroDeviceNumIsKept(t *testing.T) {
	var received commandRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/tasks" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
		}
		decodeRequest(t, request, &received)
		writeJSON(t, writer, http.StatusAccepted, map[string]any{
			"task_id":  "abc123",
			"position": 1,
		})
	}))
	defer server.Close()

	application, _, errOut := testApplication(server.URL)
	code := application.run([]string{"submit", "--device-num", "0", "--", "echo", "hi"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if received.DeviceNum != 0 {
		t.Fatalf("explicit --device-num 0 must be kept, got %+v", received)
	}
}

func TestSubmitPriority(t *testing.T) {
	var received commandRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		decodeRequest(t, request, &received)
		writeJSON(t, writer, http.StatusAccepted, map[string]any{
			"task_id":  "abc123",
			"position": 1,
			"priority": 1,
		})
	}))
	defer server.Close()

	application, out, errOut := testApplication(server.URL)
	code := application.run([]string{
		"submit", "--device-num", "1", "--priority", "1", "--", "echo", "hi",
	})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if received.Priority != 1 {
		t.Fatalf("unexpected priority: %+v", received)
	}
	if !strings.Contains(out.String(), "priority: 1") {
		t.Fatalf("missing priority notice: %s", out.String())
	}

	// 不带 --priority 时 priority 应为 0
	received = commandRequest{}
	code = application.run([]string{
		"submit", "--device-num", "1", "--", "echo", "hi",
	})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if received.Priority != 0 {
		t.Fatalf("unexpected priority: %+v", received)
	}

	// 非法 priority（负数）在客户端被拒绝
	code = application.run([]string{
		"submit", "--device-num", "1", "--priority", "-1", "--", "echo", "hi",
	})
	if code == 0 {
		t.Fatalf("negative priority should be rejected: %s", out.String())
	}
}

func TestSubmitRequiresCommandSeparator(t *testing.T) {
	application, _, errOut := testApplication("http://127.0.0.1:1")
	code := application.run([]string{"submit", "--device-num", "1", "echo ok"})
	if code != 2 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(errOut.String(), "命令必须放在 -- 后") {
		t.Fatalf("unexpected stderr: %s", errOut.String())
	}
}

func TestSubmitRejectsDeviceNumberWithExplicitDevice(t *testing.T) {
	application, _, errOut := testApplication("http://127.0.0.1:1")
	code := application.run([]string{
		"submit", "--device-num", "1", "--device", "3", "--", "echo", "ok",
	})
	if code != 2 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(errOut.String(), "互斥") {
		t.Fatalf("unexpected stderr: %s", errOut.String())
	}
}
