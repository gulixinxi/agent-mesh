package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mk 在 base 下建目录（含父级），失败即终止测试。
func mk(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(parts...)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("建目录失败 %s: %v", p, err)
	}
	return p
}

func TestExpandPathMissingBaseSkipsCandidate(t *testing.T) {
	bases := map[string]string{"local": "/tmp/does-not-matter"}
	if _, ok := expandPath(bases, "{roaming}/Doubao"); ok {
		t.Fatal("base 缺失时必须跳过，否则会退化成扫描根目录")
	}
	if _, ok := expandPath(bases, "{nosuch}/X"); ok {
		t.Fatal("未知占位符必须跳过")
	}
	got, ok := expandPath(bases, "{local}/Doubao")
	if !ok {
		t.Fatal("已知 base 应展开成功")
	}
	if filepath.ToSlash(got[len(bases["local"]):]) != "/Doubao" {
		t.Fatalf("展开结果不对: %q", got)
	}
	if _, ok := expandPath(bases, "{broken"); ok {
		t.Fatal("占位符未闭合必须跳过")
	}
}

func TestScanDetectsInstalledApps(t *testing.T) {
	root := t.TempDir()
	bases := map[string]string{
		"programs": filepath.Join(root, "Programs"),
		"local":    filepath.Join(root, "Local"),
		"roaming":  filepath.Join(root, "Roaming"),
	}
	mk(t, bases["programs"], "Trae CN")
	mk(t, bases["local"], "Doubao")
	mk(t, bases["roaming"], "Cursor")

	got := scanAIAppsWith(bases)
	kinds := map[string]bool{}
	for _, a := range got {
		kinds[a.Kind] = true
	}
	for _, want := range []string{"trae", "doubao", "cursor"} {
		if !kinds[want] {
			t.Fatalf("应探测到 %s，实际清单: %v", want, kinds)
		}
	}
	if len(got) != 3 {
		t.Fatalf("只建了 3 个目录，不该报出 %d 个: %+v", len(got), got)
	}
}

func TestScanEmptyBasesDetectsNothing(t *testing.T) {
	if got := scanAIAppsWith(map[string]string{}); len(got) != 0 {
		t.Fatalf("base 全空时不应有任何命中: %+v", got)
	}
}

func TestScanReportsVersionFromPackageJSON(t *testing.T) {
	root := t.TempDir()
	bases := map[string]string{"programs": filepath.Join(root, "Programs")}
	app := mk(t, bases["programs"], "Cursor", "resources", "app")
	body := `{"name":"cursor","version":"1.2.345","foo":"bar"}`
	if err := os.WriteFile(filepath.Join(app, "package.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := scanAIAppsWith(bases)
	if len(got) != 1 || got[0].Kind != "cursor" {
		t.Fatalf("期望只命中 cursor，实际: %+v", got)
	}
	if got[0].Version != "1.2.345" {
		t.Fatalf("版本号应读出 1.2.345，实际 %q", got[0].Version)
	}
}

func TestScanVersionEmptyWhenNoPackageJSON(t *testing.T) {
	root := t.TempDir()
	bases := map[string]string{"local": filepath.Join(root, "Local")}
	mk(t, bases["local"], "Doubao")
	got := scanAIAppsWith(bases)
	if len(got) != 1 {
		t.Fatalf("期望 1 个命中，实际 %d", len(got))
	}
	if got[0].Version != "" {
		t.Fatalf("读不到版本时应留空，不该编造: %q", got[0].Version)
	}
}

// TestMeasuredAppsAreMarkedEncrypted 是 G-1 结论的回归护栏：
// 豆包 / Trae / ChatGPT 都已经实测证明读不到正文，若有人把它们改成 plain，
// 控制台就会给出虚假的采集能见度。
func TestMeasuredAppsAreMarkedEncrypted(t *testing.T) {
	want := map[string]Collectability{
		"doubao":  CollectEncrypted,
		"trae":    CollectEncrypted,
		"chatgpt": CollectEncrypted,
		"ollama":  CollectAPI,
	}
	found := map[string]Collectability{}
	for _, spec := range aiAppCatalog {
		found[spec.Kind] = spec.Collectable
	}
	for kind, c := range want {
		if found[kind] != c {
			t.Fatalf("%s 应为 %s，实际 %s", kind, c, found[kind])
		}
	}
}

func TestCatalogKindsUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, spec := range aiAppCatalog {
		if seen[spec.Kind] {
			t.Fatalf("kind 重复会导致探测结果互相覆盖: %s", spec.Kind)
		}
		seen[spec.Kind] = true
		if len(spec.Paths) == 0 {
			t.Fatalf("%s 没有候选路径，永远探测不到", spec.Kind)
		}
	}
}

func TestScanSortedAndDeduplicated(t *testing.T) {
	root := t.TempDir()
	bases := map[string]string{"local": filepath.Join(root, "Local"), "roaming": filepath.Join(root, "Roaming")}
	// 同一 kind 在两处同时命中，应只报一条。
	mk(t, bases["local"], "Doubao")
	mk(t, bases["roaming"], "Doubao")

	got := scanAIAppsWith(bases)
	if len(got) != 1 {
		t.Fatalf("同 kind 多处命中应去重，实际 %d 条: %+v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Kind > got[i].Kind {
			t.Fatalf("结果未按 kind 排序: %+v", got)
		}
	}
}

func TestScanAIAppsUsesCache(t *testing.T) {
	appCacheMu.Lock()
	appCache = []DetectedApp{{Kind: "cached"}}
	appCacheAt = time.Now()
	appCacheMu.Unlock()

	got := ScanAIApps()
	if len(got) != 1 || got[0].Kind != "cached" {
		t.Fatalf("TTL 内应直接返回缓存，实际: %+v", got)
	}

	appCacheMu.Lock()
	appCache = nil
	appCacheAt = time.Time{}
	appCacheMu.Unlock()
}
