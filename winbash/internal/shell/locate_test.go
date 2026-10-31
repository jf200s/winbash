package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"winbash/internal/errs"
)

// locate 单入口与负结果缓存的单元测试。
// 行为不变(④ 三行式、command -v、③ 层执行)由既有测试钉:classify_test.go、
// commandv_test.go、scriptname_test.go 等;这里只钉新增的三件事:
// 定位结果随层一起返回、负缓存命中、缓存键隔离。

// TestLocateReturnsPathWithAbsForBareName 钉「(层, 绝对路径)一起带走」这个契约:
// runExternal 对裸名字不再自己扫,拿的就是这里返回的 abs。
func TestLocateReturnsPathWithAbsForBareName(t *testing.T) {
	dir := t.TempDir()
	name := "wb-locate-hit-cmd"
	if err := os.WriteFile(filepath.Join(dir, name+".exe"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	lay, abs := locate(name, dir)
	if lay != layerPath {
		t.Fatalf("lay=%v want layerPath", lay)
	}
	if want := filepath.Join(dir, name+".exe"); abs != want {
		t.Fatalf("abs=%q want %q", abs, want)
	}
}

// TestLocateRecordsAndReusesMiss 钉负缓存的两个面:
//  1. miss 被记录(locateMissKnown 变真);
//  2. 第二次查询**走的是缓存**而不是重扫 —— 判据:第一次 miss 之后把文件造出来,
//     若还在重扫就会翻成命中;仍答 miss 才证明缓存被 consult 了。
//     这同时就是文档里那条盲区(会话中途装新工具)的钉法。
func TestLocateRecordsAndReusesMiss(t *testing.T) {
	dir := t.TempDir()
	name := "wb-locate-miss-cmd"

	if lay, abs := locate(name, dir); lay != layerMissing || abs != "" {
		t.Fatalf("first lookup: lay=%v abs=%q want (layerMissing, \"\")", lay, abs)
	}
	if !locateMissKnown(name, dir) {
		t.Fatal("miss was not recorded")
	}

	// 会话中途装了新工具:文件此刻真的存在了。
	if err := os.WriteFile(filepath.Join(dir, name+".exe"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	lay, _ := locate(name, dir)
	if lay != layerMissing {
		t.Fatal("negative cache was not consulted on the second lookup (got a fresh hit)")
	}
}

// TestLocateCacheKeyIsSearchPath 钉键隔离:同一条命令在 A 路径下 miss,
// 不得污染 B 路径下的判定。这就是「键 = 完整 searchPath 串」的验收。
func TestLocateCacheKeyIsSearchPath(t *testing.T) {
	dirA := t.TempDir() // 没有这个命令
	dirB := t.TempDir()
	name := "wb-locate-iso-cmd"
	if err := os.WriteFile(filepath.Join(dirB, name+".exe"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if lay, _ := locate(name, dirA); lay != layerMissing {
		t.Fatalf("dirA: lay=%v want layerMissing", lay)
	}
	lay, abs := locate(name, dirB)
	if lay != layerPath {
		t.Fatalf("dirB: lay=%v want layerPath (A 的 miss 串键了)", lay)
	}
	if want := filepath.Join(dirB, name+".exe"); abs != want {
		t.Fatalf("dirB: abs=%q want %q", abs, want)
	}
}

// TestLocatePathFormBypassesNegativeCache 钉「路径写法不进负缓存」:
// `./x` 这类 miss 与 cwd 相关,而缓存键里没有 cwd —— 记了会毒化别的目录。
func TestLocatePathFormBypassesNegativeCache(t *testing.T) {
	dir := t.TempDir()
	name := "wb-locate-pathform.sh"
	if lay, _ := locate("./"+name, dir); lay != layerMissing {
		t.Fatalf("path-form miss: lay=%v want layerMissing", lay)
	}
	if locateMissKnown("./"+name, dir) {
		t.Fatal("path-form miss must not be recorded in the cache")
	}
}

// TestLocateShortCircuitsAtUpperLayers 钉 ①/② 的优先级短路:这两层命中后
// 连负缓存都不该问(更不该扫 PATH)。
func TestLocateShortCircuitsAtUpperLayers(t *testing.T) {
	if lay, abs := locate("ls", ""); lay != layerBuiltin || abs != "" {
		t.Fatalf("ls: lay=%v abs=%q want (layerBuiltin, \"\")", lay, abs)
	}
	if lay, abs := locate("ps", ""); lay != layerWinmgmt || abs != "" {
		t.Fatalf("ps: lay=%v abs=%q want (layerWinmgmt, \"\")", lay, abs)
	}
}

// TestCachedMissingCommandStillErrorsLikeBefore 钉缓存命中的 miss 走完 ④ 层的
// 全套产出:三行式错误 + exit 127,与未命中时逐字同型(第一次调用时记录,
// 第二次调用时命中缓存 —— 两条断言在同一个用例里对出)。
func TestCachedMissingCommandStillErrorsLikeBefore(t *testing.T) {
	s := NewSession()
	defer s.Close()
	name := "wb-cached-miss-cmd"

	_, exit1, _ := s.Run(context.Background(), name, false)
	if exit1 != errs.ExitNotFound {
		t.Fatalf("first run: exit=%d want %d", exit1, errs.ExitNotFound)
	}

	_, exit2, errs2 := s.Run(context.Background(), name, false)
	if exit2 != errs.ExitNotFound {
		t.Fatalf("second run (cache hit): exit=%d want %d", exit2, errs.ExitNotFound)
	}
	joined := strings.Join(errs2, "\n")
	if !strings.Contains(joined, name) {
		t.Fatalf("cache-hit error must still name the command: %q", joined)
	}
	if n := strings.Count(joined, "\n"); n != 2 {
		t.Fatalf("cache-hit error must stay three lines, got %d: %q", n+1, joined)
	}
}
