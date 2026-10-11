package main

// Корень работы — то, к чему привязана память. Права выдаются проекту, а
// не каждой его подпапке, поэтому и хук, и инструмент памяти должны
// назвать одну и ту же папку из любого места внутри неё.
//
// Правило переехало сюда из двух почти одинаковых кусков shell (у Claude
// Code и у Codex): одно место, один тест.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WorkRoot — корень работы для папки. Сначала git, потом сама папка.
// Зонтик из нескольких репозиториев (у нас это server, app и agent под
// общим CLAUDE.md) — один проект, а не три: если у родителя лежит
// CLAUDE.md и сам он не под git, память привязывается к нему.
func WorkRoot(dir string) string {
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			dir = wd
		}
	}
	if dir == "" {
		return ""
	}
	root := gitRoot(dir)
	if root == "" {
		root = dir
	}
	parent := filepath.Dir(root)
	if parent != root && hasFile(parent, "CLAUDE.md") && !hasDir(parent, ".git") {
		root = parent
	}
	return root
}

func gitRoot(dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func hasFile(dir, name string) bool {
	st, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !st.IsDir()
}

func hasDir(dir, name string) bool {
	st, err := os.Stat(filepath.Join(dir, name))
	return err == nil && st.IsDir()
}
