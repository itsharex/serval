package sshconn

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"serval/internal/devsshd"
)

func TestReadLimitedRejectsBinaryHugeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	text := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(text, []byte("你好\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin.dat")
	if err := os.WriteFile(bin, []byte{'a', 0, 'b'}, 0o644); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(big, []byte("abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(text, link); err != nil {
		t.Fatal(err)
	}

	files := editFiles(t)
	got, _, err := files.ReadLimited(context.Background(), text, 100)
	if err != nil || string(got) != "你好\n" {
		t.Fatalf("文本 = %q, %v", got, err)
	}
	if _, _, err := files.ReadLimited(context.Background(), bin, 100); !errors.Is(err, ErrNotText) {
		t.Fatalf("二进制应拒绝，err=%v", err)
	}
	if _, _, err := files.ReadLimited(context.Background(), big, 3); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("超限应拒绝且不读正文，err=%v", err)
	}
	if _, _, err := files.ReadLimited(context.Background(), link, 100); err == nil {
		t.Fatal("符号链接不应打开")
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := files.ReadLimited(context.Background(), sub, 100); err == nil {
		t.Fatal("目录不应打开")
	}
	bad := filepath.Join(dir, "bad.txt")
	if err := os.WriteFile(bad, []byte{0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := files.ReadLimited(context.Background(), bad, 100); !errors.Is(err, ErrNotText) {
		t.Fatalf("非法 UTF-8 应拒绝，err=%v", err)
	}
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, err = files.ReadLimited(context.Background(), empty, 100)
	if err != nil || len(got) != 0 {
		t.Fatalf("空文件 = %q, %v", got, err)
	}
}

func TestWriteFileDetectsRemoteChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := editFiles(t)
	data, mod, err := files.ReadLimited(context.Background(), path, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.WriteFile(context.Background(), path, []byte("new"), int64(len(data)), mod.Unix(), false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Fatalf("保存后 = %q", got)
	}
	data, mod, err = files.ReadLimited(context.Background(), path, 100)
	if err != nil {
		t.Fatal(err)
	}
	future := mod.Add(5 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	err = files.WriteFile(context.Background(), path, []byte("newer"), int64(len(data)), mod.Unix(), false)
	if !errors.Is(err, ErrRemoteChanged) {
		t.Fatalf("修改时间变了应拒绝，err=%v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Fatalf("拒绝后不应改文件，= %q", got)
	}
	if err := files.WriteFile(context.Background(), path, []byte("newer"), int64(len(data)), mod.Unix(), true); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "newer" {
		t.Fatalf("确认覆盖后 = %q", got)
	}
	data, mod, err = files.ReadLimited(context.Background(), path, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed-size"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = files.WriteFile(context.Background(), path, []byte("nope"), int64(len(data)), mod.Unix(), false)
	if !errors.Is(err, ErrRemoteChanged) {
		t.Fatalf("大小变了应拒绝，err=%v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "changed-size" {
		t.Fatalf("拒绝后不应改文件，= %q", got)
	}
}

func TestReadTailLimited(t *testing.T) {
	files := editFiles(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	var content string
	for i := 1; i <= 20; i++ {
		content += "line-" + string(rune('0'+i/10)) + string(rune('0'+i%10)) + "\n"
	}
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	// 大于 limit 时只读末尾并跳过截断的首行残片
	got, mod, truncated, err := files.ReadTailLimited(context.Background(), logPath, 30)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || mod.IsZero() {
		t.Fatalf("truncated=%v mod=%v, want true non-zero", truncated, mod)
	}
	if !bytes.Contains(got, []byte("line-20\n")) || bytes.Contains(got, []byte("line-01\n")) {
		t.Fatalf("tail 内容异常: %q", got)
	}

	// 小于 limit 时完整返回且 truncated=false
	smallPath := filepath.Join(dir, "small.log")
	if err := os.WriteFile(smallPath, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, truncated, err = files.ReadTailLimited(context.Background(), smallPath, 100)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || string(got) != "hello\nworld\n" {
		t.Fatalf("小文件读取异常: truncated=%v got=%q", truncated, got)
	}
}

func editFiles(t *testing.T) *Files {
	t.Helper()
	server := startServer(t, devsshd.Options{})
	client := mustDial(t, dialConfig(server, acceptAll(t)))
	files, err := client.OpenFiles()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	return files
}
