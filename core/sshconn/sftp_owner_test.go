package sshconn

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"serval/internal/devsshd"
)

func TestUploadOwnedPreservesModeAndAppliesOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX 权限与属主测试仅在类 Unix 系统运行")
	}
	remoteDir := t.TempDir()
	remoteFile := filepath.Join(remoteDir, "script.sh")
	if err := os.WriteFile(remoteFile, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	localFile := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(localFile, []byte("new-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	server := startServer(t, devsshd.Options{})
	client := mustDial(t, dialConfig(server, acceptAll(t)))
	files, err := client.OpenFiles()
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()

	owner := &FileOwner{UID: os.Getuid(), GID: os.Getgid()}
	chownErr, err := files.UploadOwned(context.Background(), localFile, filepath.ToSlash(remoteFile), owner, nil)
	if err != nil {
		t.Fatalf("UploadOwned failed: %v", err)
	}
	if chownErr != nil {
		t.Fatalf("UploadOwned returned unexpected chownErr for current uid/gid: %v", chownErr)
	}

	info, err := os.Stat(remoteFile)
	if err != nil {
		t.Fatal(err)
	}
	if gotPerm := info.Mode().Perm(); gotPerm != 0o755 {
		t.Fatalf("覆盖上传应保留原文件权限 0755，实际 %04o", gotPerm)
	}
	got, err := os.ReadFile(remoteFile)
	if err != nil || string(got) != "new-content" {
		t.Fatalf("文件内容不对: got=%q err=%v", got, err)
	}

	if os.Geteuid() != 0 {
		// 非 root 用户尝试 chown 到 root(0:0) 应返回 chownErr，但文件本身仍应上传成功。
		otherFile := filepath.Join(remoteDir, "nonroot.txt")
		chownErr, err := files.UploadOwned(context.Background(), localFile, filepath.ToSlash(otherFile), &FileOwner{UID: 0, GID: 0}, nil)
		if err != nil {
			t.Fatalf("chown 失败不应中断文件上传: %v", err)
		}
		if chownErr == nil {
			t.Fatal("非 root 用户 chown 到 0:0 应返回 chownErr")
		}
		if _, err := os.Stat(otherFile); err != nil {
			t.Fatalf("chown 失败时文件仍应落盘: %v", err)
		}
	}
}

func TestMkdirAllOwnedCreatesDirectoriesWithOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX 属主测试仅在类 Unix 系统运行")
	}
	remoteDir := t.TempDir()
	existingParent := filepath.Join(remoteDir, "existing")
	if err := os.MkdirAll(existingParent, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(existingParent, "nested", "deep")

	server := startServer(t, devsshd.Options{})
	client := mustDial(t, dialConfig(server, acceptAll(t)))
	files, err := client.OpenFiles()
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()

	owner := &FileOwner{UID: os.Getuid(), GID: os.Getgid()}
	chownErr, err := files.MkdirAllOwned(filepath.ToSlash(target), owner)
	if err != nil {
		t.Fatalf("MkdirAllOwned failed: %v", err)
	}
	if chownErr != nil {
		t.Fatalf("MkdirAllOwned returned unexpected chownErr: %v", chownErr)
	}
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		t.Fatalf("目标目录未创建: info=%v err=%v", info, err)
	}
}
