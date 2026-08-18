package auditlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestDisabledWriterNeverTouchesFilesystemOrRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.jsonl")
	w, err := Open(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Append(func() {}); err != nil { // not JSON-marshalable; disabled must not inspect it
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disabled audit touched filesystem: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"missing path", Config{Enabled: true}},
		{"relative path", Config{Enabled: true, Path: "audit.jsonl"}},
		{"negative bytes", Config{Enabled: true, Path: "/audit.jsonl", MaxFileBytes: -1}},
		{"negative files", Config{Enabled: true, Path: "/audit.jsonl", MaxFiles: -1}},
		{"too many files", Config{Enabled: true, Path: "/audit.jsonl", MaxFiles: 101}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestWriterProducesExactOwnerOnlyJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := Open(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(map[string]any{"schema_version": 1, "provider": "anthropic"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{\"provider\":\"anthropic\",\"schema_version\":1}\n" {
		t.Fatalf("line = %q", b)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

func TestWriterRejectsUnsafeExistingTargets(t *testing.T) {
	dir := t.TempDir()
	worldReadable := filepath.Join(dir, "world.jsonl")
	if err := os.WriteFile(worldReadable, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(worldReadable, symlink); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, worldReadable, symlink} {
		if _, err := Open(Config{Enabled: true, Path: path}); err == nil {
			t.Fatalf("unsafe target %q accepted", path)
		}
	}
}

func TestRotationIsBoundedAndKeepsWholeLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := Open(Config{Enabled: true, Path: path, MaxFileBytes: 20, MaxFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		if err := w.Append(map[string]int{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	wantBySuffix := map[string][]int{"": {4, 5}, ".1": {2, 3}, ".2": {0, 1}}
	for _, suffix := range []string{"", ".1", ".2"} {
		f, err := os.Open(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		s := bufio.NewScanner(f)
		var got []int
		for s.Scan() {
			var record map[string]int
			if err := json.Unmarshal(s.Bytes(), &record); err != nil {
				t.Fatalf("%s contains partial line %q: %v", suffix, s.Bytes(), err)
			}
			got = append(got, record["i"])
		}
		if err := s.Err(); err != nil {
			t.Fatal(err)
		}
		f.Close()
		want := wantBySuffix[suffix]
		if len(got) != len(want) {
			t.Fatalf("%s records = %v, want %v", suffix, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s records = %v, want %v", suffix, got, want)
			}
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("rotation exceeded bound: %v", err)
	}
}

func TestOversizedSingleRecordIsKeptWholeThenRotated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := Open(Config{Enabled: true, Path: path, MaxFileBytes: 4, MaxFiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(map[string]string{"value": "larger-than-limit"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(map[string]int{"next": 1}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", ".1"} {
		b, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) == 0 || b[len(b)-1] != '\n' || !json.Valid(b[:len(b)-1]) {
			t.Fatalf("%s does not contain one whole record: %q", suffix, b)
		}
	}
}

func TestRotationFailureBecomesStableErrorWithoutPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := Open(Config{Enabled: true, Path: path, MaxFileBytes: 8, MaxFiles: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(map[string]int{"i": 1}); err != nil {
		t.Fatal(err)
	}
	// Rotation cannot rename the active file over a non-empty directory.
	if err := os.Mkdir(path+".1", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path+".1", "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	first := w.Append(map[string]int{"i": 2})
	if first == nil {
		t.Fatal("rotation failure was ignored")
	}
	second := w.Append(map[string]int{"i": 3})
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("second append = %v, want stable %v", second, first)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentAppendsDoNotInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := Open(Config{Enabled: true, Path: path, MaxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	const count = 100
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.Append(map[string]int{"i": i}); err != nil {
				t.Errorf("append: %v", err)
			}
		}()
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	seen := make(map[int]bool, count)
	s := bufio.NewScanner(f)
	for s.Scan() {
		var record map[string]int
		if err := json.Unmarshal(s.Bytes(), &record); err != nil {
			t.Fatalf("partial/interleaved line %q: %v", s.Bytes(), err)
		}
		seen[record["i"]] = true
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != count {
		t.Fatalf("records = %d, want %d", len(seen), count)
	}
}
