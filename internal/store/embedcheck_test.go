package store

import (
	"io/fs"
	"testing"
)

func TestEmbedIncludesGC(t *testing.T) {
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range names {
		if n == "migrations/003_gc_cascade.sql" {
			found = true
		}
	}
	if !found {
		t.Fatalf("003 未嵌入: %v", names)
	}
}
