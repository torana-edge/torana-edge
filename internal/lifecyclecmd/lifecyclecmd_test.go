package lifecyclecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/torana-edge/torana-edge/internal/controlclient"
)

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
