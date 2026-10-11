package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Корень работы — то, к чему привязана память. Две ветки: обычный
// репозиторий и зонтик из нескольких репозиториев под общим CLAUDE.md.
func TestWorkRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("без git проверять нечего")
	}
	base := t.TempDir()
	repo := filepath.Join(base, "проект")
	sub := filepath.Join(repo, "internal", "глубже")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	got := WorkRoot(sub)
	if !sameDir(got, repo) {
		t.Fatalf("из подпапки получили %q, ждали %q", got, repo)
	}

	// зонтик: у родителя лежит CLAUDE.md и он сам не под git — значит
	// это один проект из трёх репозиториев, а не три проекта
	zont := filepath.Join(base, "зонтик")
	inner := filepath.Join(zont, "server")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zont, "CLAUDE.md"), []byte("общий"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", inner, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if got := WorkRoot(inner); !sameDir(got, zont) {
		t.Fatalf("зонтик не опознан: получили %q, ждали %q", got, zont)
	}

	// а под git родитель остаётся родителем: чужой корень не забираем
	if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("свой"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := WorkRoot(sub); !sameDir(got, repo) {
		t.Fatalf("корень уехал вверх: %q", got)
	}
}

func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return ra == rb
}
