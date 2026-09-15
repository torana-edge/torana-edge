package lifecyclecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/controlclient"
)

func TestStatusReadableByDefaultAndJSONOnRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Status{Service: "torana-edge", InstanceID: "test", PID: 123, Status: "running", ConfigPath: "/config.json", UptimeSeconds: 75})
	}))
	defer srv.Close()
	for _, jsonOutput := range []bool{false, true} {
		args := []string{"status", "--addr", srv.URL}
		if jsonOutput {
			args = append(args, "--json")
		}
		var out, diag bytes.Buffer
		if err := Run(context.Background(), args, &out, &diag); err != nil {
			t.Fatal(err)
		}
		if jsonOutput {
			var status Status
			if err := json.Unmarshal(out.Bytes(), &status); err != nil || status.PID != 123 {
				t.Fatalf("JSON: %s, %v", &out, err)
			}
			if !strings.Contains(out.String(), "\n  \"service\"") {
				t.Fatal("JSON is not indented")
			}
		} else {
			for _, want := range []string{"running", "Control plane", srv.URL + "/_torana/", "1m15s"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing %q: %s", want, &out)
				}
			}
			if json.Valid(out.Bytes()) {
				t.Fatal("default is still JSON")
			}
		}
	}
}

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, errors.New("closed output") }

func TestStoppedOutputAndWriteFailures(t *testing.T) {
	var out bytes.Buffer
	s := Status{Status: "stopped", Address: "http://127.0.0.1:8080", PID: 123}
	if err := printStatus(&out, s, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "stopped") || strings.Contains(out.String(), "Control plane") || strings.Contains(out.String(), "PID") {
		t.Fatal(out.String())
	}
	for _, jsonOutput := range []bool{false, true} {
		if err := printStatus(failedWriter{}, s, jsonOutput); err == nil {
			t.Fatal("output failure ignored")
		}
	}
}

func TestStopRequiresConsentAndIdentity(t *testing.T) {
	var mutations int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations++
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"service": "something-else", "pid": 123, "instance_id": "wrong", "config_path": "/different"})
	}))
	defer srv.Close()
	for _, args := range [][]string{{"stop", "--addr", srv.URL}, {"stop", "--addr", srv.URL, "--yes"}} {
		var out, diag bytes.Buffer
		if err := Run(context.Background(), args, &out, &diag); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if mutations != 0 {
		t.Fatal("sent stop without verified identity and consent")
	}
}

func TestInspectAndShutdownBindExactInstance(t *testing.T) {
	t.Setenv("TORANA_DATA_DIR", t.TempDir())
	var gotStop bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Torana-Local-Request") != "1" {
			t.Error("missing local automation header")
		}
		if strings.HasSuffix(r.URL.Path, "/stop") {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["instance_id"] != "original" {
				t.Errorf("wrong target: %v", body)
			}
			gotStop = true
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"stopping"}`))
			return
		}
		id := "original"
		if gotStop {
			id = "replacement"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"service": "torana-edge", "pid": 123, "instance_id": id, "status": "running", "config_path": "/different"})
	}))
	defer srv.Close()
	c, err := controlclient.New(srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := Inspect(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stop(context.Background(), c, s); err == nil || !strings.Contains(err.Error(), "another instance") {
		t.Fatalf("replacement not protected: %v", err)
	}
}
