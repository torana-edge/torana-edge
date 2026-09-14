package pluginstate

import (
	"errors"
	"path/filepath"
	"testing"
)

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
