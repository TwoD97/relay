package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TwoD97/relay/internal/controller"
)

type lostHandoverResponse struct{ headers http.Header }

func (w *lostHandoverResponse) Header() http.Header       { return w.headers }
func (w *lostHandoverResponse) WriteHeader(int)           {}
func (w *lostHandoverResponse) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (w *lostHandoverResponse) Flush()                    {}

func TestControllerLostHandoverResponseDoesNotRepeatStop(t *testing.T) {
	dir := t.TempDir()
	app, err := controller.New(controller.Config{StateDir: dir, Address: "127.0.0.1:43210", Version: version})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	info := controllerInfo{PID: os.Getpid(), Version: version, Address: "http://127.0.0.1:43210", Protocol: 1}
	var stops atomic.Int32
	handler := controllerControlHandler(app, info, uiOptions{StateDir: dir}, func() { stops.Add(1) })
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	var health controllerHealth
	if err := json.Unmarshal(w.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(controllerHandoverRequest{PID: info.PID, Version: version, Instance: health.Instance})
	handler.ServeHTTP(&lostHandoverResponse{headers: make(http.Header)}, httptest.NewRequest("POST", "/handover", bytes.NewReader(data)))
	if stops.Load() != 1 {
		t.Fatal("accepted handover was undone by response loss")
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/handover", bytes.NewReader(data)))
	if w.Code != 409 || stops.Load() != 1 {
		t.Fatal("retry after lost acknowledgement repeated shutdown")
	}
}

func stopHandoverTestController(p *controllerProbe, opts uiOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if p.health.Handover == 1 {
		var ack struct {
			Accepted bool `json:"accepted"`
		}
		if err := p.request(ctx, http.MethodPost, "/handover", controllerHandoverRequest{PID: p.health.PID, Version: p.health.Version, Instance: p.health.Instance}, &ack); err != nil {
			return err
		}
	} else {
		if err := p.process.Verify(ctx, p.health, opts); err != nil {
			return err
		}
		if err := p.process.StopLegacy(opts.StateDir); err != nil {
			return err
		}
	}
	return p.process.Wait(ctx)
}

func TestControllerHandoverRequiresExactIdentityAndNeverRepeatsShutdown(t *testing.T) {
	dir := t.TempDir()
	app, err := controller.New(controller.Config{StateDir: dir, Address: "127.0.0.1:43210", Version: version})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	info := controllerInfo{Address: "http://127.0.0.1:43210", Version: version, Protocol: controllerProtocol, PID: os.Getpid()}
	var stops atomic.Int32
	handler := controllerControlHandler(app, info, uiOptions{StateDir: dir}, func() { stops.Add(1) })
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	var health controllerHealth
	if err := json.Unmarshal(w.Body.Bytes(), &health); err != nil || health.Handover != 1 || health.Instance == "" {
		t.Fatal("missing handover capability", err)
	}
	valid := controllerHandoverRequest{PID: info.PID, Version: version, Instance: health.Instance}
	for _, mutate := range []func(*controllerHandoverRequest){
		func(r *controllerHandoverRequest) { r.PID++ },
		func(r *controllerHandoverRequest) { r.Version += ".wrong" },
		func(r *controllerHandoverRequest) { r.Instance += "wrong" },
	} {
		r := valid
		mutate(&r)
		data, _ := json.Marshal(r)
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/handover", bytes.NewReader(data)))
		if w.Code != 409 || stops.Load() != 0 {
			t.Fatal("invalid identity stopped the sentinel", w.Code, stops.Load())
		}
	}
	for _, body := range []string{`{"pid":1,"extra":true}`, strings.Repeat("x", 2049), `{}`} {
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/handover", strings.NewReader(body)))
		if w.Code < 400 || stops.Load() != 0 {
			t.Fatal("invalid request stopped the sentinel")
		}
	}
	data, _ := json.Marshal(valid)
	for i := 0; i < 2; i++ {
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/handover", bytes.NewReader(data)))
		if (i == 0 && w.Code != 200) || (i == 1 && w.Code != 409) || stops.Load() != 1 {
			t.Fatal("handover must happen exactly once", w.Code, stops.Load())
		}
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/launch", nil))
	if w.Code != 503 {
		t.Fatal("shutting-down controller issued another login")
	}
}

func TestControllerArgumentsAndReleaseMustMatchPrivateState(t *testing.T) {
	dir := t.TempDir()
	opts := uiOptions{StateDir: filepath.Join(dir, "state"), RuntimeDir: filepath.Join(dir, "runtime")}
	health := controllerHealth{RuntimeDir: opts.RuntimeDir}
	args := []string{"relay", "ui", "--state-dir", opts.StateDir, "--runtime-dir", opts.RuntimeDir, "--local=false"}
	if err := validateControllerArguments(args, health, opts); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]string{
		{"relay", "daemon", "--state-dir", opts.StateDir},
		{"relay", "ui", "--runtime-dir", opts.RuntimeDir, "--local=false"},
		{"relay", "ui", "--state-dir", dir, "--runtime-dir", opts.RuntimeDir, "--local=false"},
		{"relay", "ui", "--state-dir", opts.StateDir, "--runtime-dir", dir, "--local=false"},
	} {
		if err := validateControllerArguments(invalid, health, opts); err == nil {
			t.Fatal("accepted wrong process arguments", invalid)
		}
	}
	exe := filepath.Join(dir, "source-controller")
	if err := os.WriteFile(exe, []byte("fixture bytes"), 0700); err != nil {
		t.Fatal(err)
	}
	opts.Executable, opts.BinaryDir = exe, dir
	staged, err := stageController(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyControllerRelease(staged.Executable, opts.StateDir); err != nil {
		t.Fatal(err)
	}
	if err := verifyControllerRelease(staged.Executable, dir); err == nil {
		t.Fatal("accepted another controller state")
	}
	if err := verifyControllerRelease(exe, opts.StateDir); err == nil {
		t.Fatal("accepted an unstaged executable")
	}
	if err := os.WriteFile(staged.Executable, []byte("corrupt bytes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := verifyControllerRelease(staged.Executable, opts.StateDir); err == nil {
		t.Fatal("accepted corrupted executable under old release key")
	}
}
