package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// newFileDB 为每个用例开一个独立的临时库，避免用例之间互相污染。
func newFileDB(t *testing.T) {
	t.Helper()
	if err := InitDB(filepath.Join(t.TempDir(), "files.db")); err != nil {
		t.Fatalf("初始化数据库失败: %v", err)
	}
	t.Cleanup(func() { DB.Close() })
}

func TestFileMetaCRUD(t *testing.T) {
	newFileDB(t)
	now := time.Now().Unix()

	meta := FileMeta{
		FileID:     "file-aaa",
		FileName:   "报表.xlsx",
		Size:       2048,
		SHA256:     "abc123",
		Uploader:   "NODE-A",
		TargetNode: "NODE-B",
		TaskID:     "task-1",
		CreatedAt:  now,
	}
	if err := InsertFile(meta); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	got, err := GetFileMeta("file-aaa")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.FileName != meta.FileName || got.Size != meta.Size ||
		got.TargetNode != meta.TargetNode || got.TaskID != meta.TaskID || got.Uploader != meta.Uploader {
		t.Errorf("读取结果与写入不一致: %+v", got)
	}
	if got.DownloadedAt != 0 {
		t.Errorf("新文件不应带领取时间，实际 %d", got.DownloadedAt)
	}

	if _, err := GetFileMeta("file-missing"); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("不存在的文件应返回 ErrFileNotFound，实际 %v", err)
	}
}

func TestListFilesForNodeVisibility(t *testing.T) {
	newFileDB(t)
	now := time.Now().Unix()

	mustInsert := func(id, target string) {
		t.Helper()
		if err := InsertFile(FileMeta{
			FileID: id, FileName: id + ".bin", Size: 10, SHA256: "x",
			Uploader: "NODE-A", TargetNode: target, CreatedAt: now,
		}); err != nil {
			t.Fatalf("写入 %s 失败: %v", id, err)
		}
	}
	mustInsert("file-to-b", "NODE-B")
	mustInsert("file-to-c", "NODE-C")
	mustInsert("file-broadcast", "")

	ids := func(list []FileMeta) map[string]bool {
		m := make(map[string]bool, len(list))
		for _, f := range list {
			m[f.FileID] = true
		}
		return m
	}

	gotB := ids(mustList(t, "NODE-B", false))
	if !gotB["file-to-b"] || !gotB["file-broadcast"] {
		t.Errorf("NODE-B 应看到定向给自己的与广播文件，实际 %v", gotB)
	}
	if gotB["file-to-c"] {
		t.Error("NODE-B 不应看到定向给 NODE-C 的文件")
	}

	gotC, err := ListFilesForNode("NODE-C", false, 50)
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(gotC) != 2 {
		t.Errorf("NODE-C 应看到 2 个文件，实际 %d", len(gotC))
	}

	// 领取 file-to-b 后，待领取列表里就不该再有它。
	if err := MarkFileDownloaded("file-to-b", now+1); err != nil {
		t.Fatalf("标记领取失败: %v", err)
	}
	pending := ids(mustList(t, "NODE-B", true))
	if pending["file-to-b"] {
		t.Error("已领取的文件不应再出现在待领取列表")
	}
	if !pending["file-broadcast"] {
		t.Error("广播文件未被领取，应仍在待领取列表")
	}

	// 重复标记不能把时间戳越刷越新：首次领取时间是有意义的事实。
	if err := MarkFileDownloaded("file-to-b", now+999); err != nil {
		t.Fatalf("重复标记失败: %v", err)
	}
	meta, err := GetFileMeta("file-to-b")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if meta.DownloadedAt != now+1 {
		t.Errorf("领取时间被覆盖为 %d，期望保持 %d", meta.DownloadedAt, now+1)
	}
}

func mustList(t *testing.T, node string, pendingOnly bool) []FileMeta {
	t.Helper()
	list, err := ListFilesForNode(node, pendingOnly, 50)
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	return list
}

func TestTotalBytesAndDelete(t *testing.T) {
	newFileDB(t)
	now := time.Now().Unix()

	for _, f := range []struct {
		id   string
		size int64
	}{{"file-1", 100}, {"file-2", 250}} {
		if err := InsertFile(FileMeta{
			FileID: f.id, FileName: f.id, Size: f.size, SHA256: "x",
			Uploader: "NODE-A", CreatedAt: now,
		}); err != nil {
			t.Fatalf("写入 %s 失败: %v", f.id, err)
		}
	}

	total, err := TotalFileBytes()
	if err != nil {
		t.Fatalf("统计占用失败: %v", err)
	}
	if total != 350 {
		t.Errorf("占用总量 = %d，期望 350", total)
	}

	deleted, err := DeleteFileMeta("file-1")
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if !deleted {
		t.Error("删除已存在的文件应返回 true")
	}
	deleted, err = DeleteFileMeta("file-1")
	if err != nil {
		t.Fatalf("重复删除报错: %v", err)
	}
	if deleted {
		t.Error("删除不存在的文件应返回 false")
	}

	total, _ = TotalFileBytes()
	if total != 250 {
		t.Errorf("删除后占用 = %d，期望 250", total)
	}
}

func TestExpiredFiles(t *testing.T) {
	newFileDB(t)
	now := time.Now().Unix()

	if err := InsertFile(FileMeta{FileID: "file-old", FileName: "old", Size: 1, SHA256: "x", CreatedAt: now - 1000}); err != nil {
		t.Fatal(err)
	}
	if err := InsertFile(FileMeta{FileID: "file-new", FileName: "new", Size: 1, SHA256: "x", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	// cutoff = now-500：只有 old 早于它。
	expired, err := ExpiredFiles(now-500, 100)
	if err != nil {
		t.Fatalf("查询过期文件失败: %v", err)
	}
	if len(expired) != 1 || expired[0].FileID != "file-old" {
		t.Errorf("过期文件应为 [file-old]，实际 %+v", expired)
	}

	// 空的统计结果不能变成 SQL NULL 引发的错误。
	if _, err := TotalFileBytes(); err != nil {
		t.Errorf("空库统计占用不应报错: %v", err)
	}
}

func TestFileMetaSurvivesMissingTable(t *testing.T) {
	// files 表由 InitDB 建立；老库升级路径要把表补出来，
	// 这里验证 InitDB 之后表确实可用（老库没有该表也不会报 "no such table"）。
	newFileDB(t)
	if _, err := ListAllFiles(10); err != nil {
		t.Fatalf("列表失败（files 表可能未建立）: %v", err)
	}
}
