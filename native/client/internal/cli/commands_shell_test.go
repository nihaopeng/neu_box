package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestShellOwnsSandboxAndReleasesAfterChildExits(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	var acquired terminalAcquireRequest
	var released map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/sandbox/acquire":
			decodeRequest(t, request, &acquired)
			writeJSON(t, writer, http.StatusCreated, map[string]any{
				"sandbox_name": "sbx_yuxd_222.slice", "devices": []string{"234:0", "234:1"},
			})
		case "/sandbox/release":
			decodeRequest(t, request, &released)
			writeJSON(t, writer, http.StatusOK, map[string]any{})
		default:
			t.Errorf("unexpected request: %s", request.URL.Path)
		}
	}))
	defer server.Close()
	application, out, errOut := testApplication(server.URL)
	application.lookPath = func(path string) (string, error) {
		if path != "/bin/zsh" {
			t.Fatalf("shell path=%q", path)
		}
		return path, nil
	}
	var childArgs []string
	application.runFn = func(_ string, argv []string, _ []string) (int, error) {
		childArgs = append([]string(nil), argv...)
		if released != nil {
			t.Fatal("released before child returned")
		}
		return 7, nil
	}
	code := application.run([]string{"shell", "--device-num", "2", "--cpu", "4", "--mem", "8"})
	if code != 7 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if acquired.PID != 222 || acquired.DeviceNum != 2 || acquired.CPU != 4 || acquired.Memory != 8 {
		t.Fatalf("acquire=%+v", acquired)
	}
	if !reflect.DeepEqual(childArgs, []string{"/bin/zsh", "-i"}) {
		t.Fatalf("child=%q", childArgs)
	}
	if released["sandbox_name"] != "sbx_yuxd_222.slice" || released["host_pid"] != float64(222) {
		t.Fatalf("release=%v", released)
	}
	if !strings.Contains(out.String(), "memory:  8G") || !strings.Contains(out.String(), "released") {
		t.Fatalf("output=%s", out.String())
	}
}

func TestShellReleasesSandboxWhenChildCannotStart(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	released := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/sandbox/acquire" {
			writeJSON(t, writer, http.StatusCreated, map[string]any{"sandbox_name": "sbx_yuxd_222.slice"})
			return
		}
		if request.URL.Path == "/sandbox/release" {
			released = true
		}
		writeJSON(t, writer, http.StatusOK, map[string]any{})
	}))
	defer server.Close()
	application, _, _ := testApplication(server.URL)
	application.runFn = func(string, []string, []string) (int, error) { return -1, errors.New("exec failed") }
	if code := application.run([]string{"shell"}); code != 1 || !released {
		t.Fatalf("exit=%d released=%v", code, released)
	}
}

func TestShellRejectsPIDAndCommandBeforeAcquiring(t *testing.T) {
	for _, args := range [][]string{{"shell", "--pid", "123"}, {"shell", "bash"}, {"shell", "--"}} {
		application, _, _ := testApplication("http://127.0.0.1:1")
		if code := application.run(args); code != 2 {
			t.Fatalf("%q exit=%d", args, code)
		}
	}
}
