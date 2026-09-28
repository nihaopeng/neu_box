package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func dockerInspectJSON(running, managed bool) []byte {
	annotation := `null`
	if managed {
		annotation = `{"sandbox_cgroup":"sbx_yuxd_old.slice"}`
	}
	state := "exited"
	if running {
		state = "running"
	}
	return []byte(fmt.Sprintf(`{"Id":%q,"State":{"Running":%t,"Status":%q},"HostConfig":{"Annotations":%s}}`,
		testContainerID, running, state, annotation))
}

func TestDockerStatusShowsCurrentAuthorization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/sandbox/status" || request.URL.Query().Get("container") != testContainerID {
			t.Errorf("unexpected query: %s", request.URL.String())
		}
		writeJSON(t, writer, http.StatusOK, map[string]any{
			"sandbox_name": "sbx_yuxd_new.slice",
			"sandbox": map[string]any{"name": "sbx_yuxd_new.slice", "state": "ACTIVE",
				"devices": []string{"234:0", "234:1"}, "cpu": 4, "mem": "8G"},
		})
	}))
	defer server.Close()
	application, out, errOut := testApplication(server.URL)
	application.outputFn = func(_ string, args ...string) ([]byte, error) {
		if !reflect.DeepEqual(args, []string{"inspect", "--format", "{{json .}}", "train"}) {
			t.Fatalf("inspect args=%q", args)
		}
		return dockerInspectJSON(true, true), nil
	}
	if code := application.run([]string{"docker", "status", "train"}); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{"container_state: running", "sandbox:", "sbx_yuxd_new.slice", "devices:", "234:0, 234:1", "cpu:", "memory:"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %s", want, out.String())
		}
	}
}

func TestDockerStatusStoppedContainerHasNoCurrentAuthorization(t *testing.T) {
	application, out, errOut := testApplication("http://127.0.0.1:1")
	application.outputFn = func(string, ...string) ([]byte, error) { return dockerInspectJSON(false, true), nil }
	if code := application.run([]string{"docker", "status", "train"}); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "managed:") || !strings.Contains(out.String(), "sandbox:") || !strings.Contains(out.String(), "none") {
		t.Fatalf("output=%s", out.String())
	}
}

func TestDockerStatusDoesNotConfuseUnavailableWorkerWithNoCards(t *testing.T) {
	application, out, errOut := testApplication("http://127.0.0.1:1")
	application.outputFn = func(string, ...string) ([]byte, error) { return dockerInspectJSON(true, true), nil }
	if code := application.run([]string{"docker", "status", "train"}); code != 1 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "unknown") || strings.Contains(out.String(), "sandbox: none") {
		t.Fatalf("unavailable Worker must be unknown: %s", out.String())
	}
}

func TestDockerRestartStopsBeforeLendingWhileOldStatusIsStale(t *testing.T) {
	stopped := false
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/sandbox/status":
			if request.URL.Query().Get("pid") == "222" {
				writeJSON(t, writer, http.StatusOK, map[string]any{"sandbox_name": "sbx_yuxd_new.slice"})
				return
			}
			if request.URL.Query().Get("container") != testContainerID {
				t.Errorf("query=%s", request.URL.String())
			}
			// The intent endpoint retires a dead registration atomically. This
			// read-only endpoint may still report the old row after Docker stop.
			writeJSON(t, writer, http.StatusOK, map[string]any{"sandbox_name": "sbx_yuxd_old.slice"})
		case "/container/intent":
			if request.Method == http.MethodPost {
				if !stopped {
					t.Error("intent created before stop")
				}
				calls = append(calls, "intent")
				var payload map[string]any
				decodeRequest(t, request, &payload)
				if payload["container_id"] != testContainerID || payload["pid"] != float64(222) {
					t.Errorf("intent=%v", payload)
				}
				writeJSON(t, writer, http.StatusOK, map[string]any{"sandbox_name": "sbx_yuxd_new.slice"})
			} else {
				writeJSON(t, writer, http.StatusOK, map[string]any{"state": "consumed", "sandbox_name": "sbx_yuxd_new.slice"})
			}
		default:
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
	}))
	defer server.Close()
	application, out, errOut := testApplication(server.URL)
	application.outputFn = func(_ string, args ...string) ([]byte, error) {
		if args[0] == "inspect" {
			return dockerInspectJSON(true, true), nil
		}
		calls = append(calls, args[0])
		if args[0] == "stop" {
			stopped = true
		}
		return []byte("train\n"), nil
	}
	if code := application.run([]string{"docker", "restart", "train"}); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !reflect.DeepEqual(calls, []string{"stop", "intent", "start"}) {
		t.Fatalf("calls=%q", calls)
	}
	if !strings.Contains(out.String(), "restarted") || !strings.Contains(out.String(), "sbx_yuxd_new.slice") {
		t.Fatalf("output=%s", out.String())
	}
}

func TestDockerRestartRejectsUnmanagedContainerBeforeStop(t *testing.T) {
	application, _, errOut := testApplication("http://127.0.0.1:1")
	application.outputFn = func(_ string, args ...string) ([]byte, error) {
		if args[0] != "inspect" {
			t.Fatal("must not stop")
		}
		return dockerInspectJSON(true, false), nil
	}
	if code := application.run([]string{"docker", "restart", "train"}); code != 2 || !strings.Contains(errOut.String(), "不受 Neu Box 管理") {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
}

func TestDockerRestartRefusesWorkerFailureBeforeStopping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("pid") == "222" {
			writeJSON(t, writer, http.StatusOK, map[string]any{"sandbox_name": "sbx_yuxd_new.slice"})
			return
		}
		writeJSON(t, writer, http.StatusServiceUnavailable, map[string]any{"error": "unavailable"})
	}))
	defer server.Close()
	application, _, _ := testApplication(server.URL)
	application.outputFn = func(_ string, args ...string) ([]byte, error) {
		if args[0] != "inspect" {
			t.Fatal("must not stop")
		}
		return dockerInspectJSON(true, true), nil
	}
	if code := application.run([]string{"docker", "restart", "train"}); code != 1 {
		t.Fatalf("exit=%d", code)
	}
}

func TestDockerRestartStopsContainerIfNewBindingCannotBeConfirmed(t *testing.T) {
	stopped := false
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/sandbox/status":
			if request.URL.Query().Get("pid") == "222" {
				writeJSON(t, writer, http.StatusOK, map[string]any{"sandbox_name": "sbx_yuxd_new.slice"})
			} else if stopped {
				writeJSON(t, writer, http.StatusOK, map[string]any{"sandbox_name": nil})
			} else {
				writeJSON(t, writer, http.StatusOK, map[string]any{"sandbox_name": "sbx_yuxd_old.slice"})
			}
		case "/container/intent":
			if request.Method == http.MethodPost {
				writeJSON(t, writer, http.StatusOK, map[string]any{"sandbox_name": "sbx_yuxd_new.slice"})
			} else {
				writeJSON(t, writer, http.StatusInternalServerError, map[string]any{"error": "hook failed"})
			}
		default:
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
	}))
	defer server.Close()
	application, _, errOut := testApplication(server.URL)
	application.outputFn = func(_ string, args ...string) ([]byte, error) {
		if args[0] == "inspect" {
			return dockerInspectJSON(true, true), nil
		}
		calls = append(calls, args[0])
		if args[0] == "stop" {
			stopped = true
		}
		return []byte("train\n"), nil
	}
	if code := application.run([]string{"docker", "restart", "train"}); code != 1 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !reflect.DeepEqual(calls, []string{"stop", "start", "stop"}) {
		t.Fatalf("calls=%q", calls)
	}
}

func TestDockerExecExplainsWhyItDoesNotRunDocker(t *testing.T) {
	application, _, errOut := testApplication("http://127.0.0.1:1")
	application.execFn = func(string, []string, []string) error { t.Fatal("must not exec docker"); return nil }
	application.runFn = func(string, []string, []string) (int, error) { t.Fatal("must not run docker"); return 0, nil }
	if code := application.run([]string{"docker", "exec", "train"}); code != 2 {
		t.Fatalf("exit=%d", code)
	}
	for _, want := range []string{"docker exec", "docker status", "neubox help verbose"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("missing %q in %s", want, errOut.String())
		}
	}
}
