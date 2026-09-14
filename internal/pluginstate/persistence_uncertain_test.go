package pluginstate

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestConditionalPersistenceDoesNotBlockReadersAndSerializesWriters(t *testing.T) {
	for _, operation := range []string{"cas", "conditional-delete"} {
		t.Run(operation, func(t *testing.T) {
			s := newStore(t, Options{Path: filepath.Join(t.TempDir(), "state.json")})
			if err := s.Set("p", "k", "old"); err != nil {
				t.Fatal(err)
			}
			_, oldVersion, _ := s.GetVersioned("p", "k")
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			var once sync.Once
			s.afterRename = func() error {
				once.Do(func() { close(entered); <-release })
				return nil
			}
			firstDone := make(chan error, 1)
			go func() {
				if operation == "cas" {
					_, _, err := s.CompareAndSet("p", "k", "conditional", &oldVersion)
					firstDone <- err
				} else {
					_, err := s.CompareAndDelete("p", "k", oldVersion)
					firstDone <- err
				}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("writer did not reach persistence")
			}
			readDone := make(chan bool, 1)
			go func() {
				got, version, ok := s.GetVersioned("p", "k")
				readDone <- ok && got == "old" && version == oldVersion
			}()
			select {
			case oldGeneration := <-readDone:
				if !oldGeneration {
					t.Fatal("reader observed uncommitted generation")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("reader blocked behind disk persistence")
			}
			secondDone := make(chan error, 1)
			go func() { secondDone <- s.Set("p", "k", "second") }()
			select {
			case err := <-secondDone:
				t.Fatalf("second writer bypassed in-flight persistence: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			unblock()
			if err := <-firstDone; err != nil {
				t.Fatal(err)
			}
			if err := <-secondDone; err != nil {
				t.Fatal(err)
			}
			got, version, ok := s.GetVersioned("p", "k")
			if !ok || got != "second" || version == oldVersion {
				t.Fatalf("final generation = %q %q %v", got, version, ok)
			}
		})
	}
}

func TestPostRenameFailureRequiresReopen(t *testing.T) {
	for _, operation := range []string{"set", "cas", "delete", "conditional-delete"} {
		t.Run(operation, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "state.json")
			s, err := New(Options{Path: file})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Set("p", "k", "old"); err != nil {
				t.Fatal(err)
			}
			_, oldVersion, _ := s.GetVersioned("p", "k")
			s.afterRename = func() error { return errors.New("injected directory sync failure") }
			switch operation {
			case "set":
				err = s.Set("p", "k", "new")
			case "cas":
				_, _, err = s.CompareAndSet("p", "k", "new", &oldVersion)
			case "delete":
				err = s.Delete("p", "k")
			case "conditional-delete":
				_, err = s.CompareAndDelete("p", "k", oldVersion)
			}
			if err == nil {
				t.Fatal("uncertain write acknowledged")
			}
			s.afterRename = nil
			if err = s.Set("p", "k", "overwrite"); err == nil {
				t.Fatal("set accepted after uncertain write")
			}
			if _, _, err = s.CompareAndSet("p", "k", "overwrite", &oldVersion); err == nil {
				t.Fatal("CAS accepted stale state")
			}
			if err = s.Delete("p", "k"); err == nil {
				t.Fatal("delete accepted after uncertain write")
			}
			if _, err = s.CompareAndDelete("p", "k", oldVersion); err == nil {
				t.Fatal("conditional delete accepted after uncertain write")
			}
			reopened, err := New(Options{Path: file})
			if err != nil {
				t.Fatal(err)
			}
			value, version, found := reopened.GetVersioned("p", "k")
			if operation == "set" || operation == "cas" {
				if !found || value != "new" || version == oldVersion {
					t.Fatalf("reopen lost renamed candidate: %q %q %v", value, version, found)
				}
			} else if found {
				t.Fatal("reopen lost committed deletion")
			}
			if _, _, err = reopened.CompareAndSet("p", "k", "fresh", nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}
