package builtins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"winbash/internal/testenv"
)

// grepClassGolden 是一行 JSONL 记录。
type grepClassGolden struct {
	Pattern string   `json:"pattern"`
	Exit    int      `json:"exit"`
	Out     []string `json:"out"`
	// Stderr 记"真 grep 有没有往 stderr 写东西"(不是内容:两侧的措辞本来就不同,
	// winbash 用中文三行式)。它是判据的一部分 —— 只看 exit 与 stdout 会漏掉
	// "打了 stderr 但退出码仍是 0"那一类形态。
	Stderr bool `json:"stderr,omitempty"`
}

// grepClassCorpus 是一字符一行的语料:grep 的**匹配结果本身就是字符集合**,
// 所以不用为每个 pattern 想一个例子,一次语料就能覆盖所有形状。
const grepClassCorpus = "\\\nd\n]\na\nz\n-\nb\nw\n[\n^\n0\n5\n"

// goldenPatterns 是要钉住的字符类形状。每一条都对应一个踩过的坑或一个必须守住的边界:
//
//	[\d] [\w]    类内的单个 `\` 在 ERE 里是**普通字符**(匹配反斜杠或字母),
//	             而 Go 的 CompilePOSIX 不认 —— 这是当初必须归一化的理由。
//	[\\d] [\\]   已经转义的 `\\`:不能再转一次,否则报非法 ERE(实测 exit 2)。
//	[\]]         类首紧跟的字面量 `]`:不能当成类的结束,否则被截断成空集合。
//	[\[] [\-a]   类内其它位置的 `\`:同样按字面处理。
//	[a\-z]       类内的 `\-`:真 grep 也不当区间(区间要么两端都裸,要么都转义)。
const grepClassPatterns = `["[\\d]","[\\w]","[\\\\d]","[\\\\]","[\\]]","[\\[]","[\\-a]","[a\\-z]","[[:alpha:]\\d]","[[:alpha:]]","[[:digit:]\\d]"]`

var updateGrepGolden = flag.Bool("update-grep-golden", false,
	"用真 Git Bash 重新生成 testdata/grep-e-classes.jsonl")

// TestGrepEClassesMatchRealGrep 把 grep -E 的字符类语义钉在真 grep 的实测结果上。
//
// 为什么这条测试重要:normalizeEreClasses 是本项目唯一"把一种语法改写成另一种再交给
// 引擎"的地方,而它的行覆盖率一度到 80% —— 覆盖率在这里**骗了人**。一次改动同时踩了
// 两个边界(重复转义 `\\`、不认类首字面量 `]`),回归后的两种表现**都是哑的**:
// 一种报"非法 ERE"(exit 2,看起来像模式写错了),一种静默无匹配(exit 1,看起来像
// "真的没有匹配")。只有把 9 个 pattern 逐个拿去和真 grep 对照,才看得见。
//
// 期望值不手写:由 TestUpdateGrepClassGolden 跑真 Git Bash 生成。手写的期望值只能覆盖
// 作者想到的形状;一字符一行的语料 + 真 grep 当 oracle 能覆盖作者没想到的形状。
func TestGrepEClassesMatchRealGrep(t *testing.T) {
	golden := loadGrepClassGolden(t)
	if len(golden) == 0 {
		t.Fatal("golden 表是空的")
	}

	dir := t.TempDir()
	writeGrepClassCorpus(t, dir)

	for _, c := range golden {
		t.Run(c.Pattern, func(t *testing.T) {
			var out, errw bytes.Buffer
			code := Grep(context.Background(), dir,
				[]string{"-E", c.Pattern, "chars.txt"},
				strings.NewReader(""), &out, &errw)

			if code != c.Exit {
				t.Fatalf("退出码 = %d, 真 grep = %d\nwinbash stderr:\n%s", code, c.Exit, errw.String())
			}
			if got := errw.Len() > 0; got != c.Stderr {
				t.Errorf("stderr 的有无与真 grep 不一致: winbash=%v 真grep=%v\nwinbash stderr:\n%s",
					got, c.Stderr, errw.String())
			}
			got := grepClassLines(out.String())
			if !slices.Equal(got, c.Out) {
				t.Errorf("匹配结果与真 grep 不一致\nwinbash: %q\n真grep : %q", got, c.Out)
			}
		})
	}
}

// TestUpdateGrepClassGolden 重新生成 golden 表。默认跳过。
//
// 需要真 Git Bash:路径取 WINBASH_REAL_BASH,缺省用 Git for Windows 的标准安装位置。
func TestUpdateGrepClassGolden(t *testing.T) {
	if !*updateGrepGolden {
		t.Skip("只在 -update-grep-golden 时重新生成")
	}
	bash := os.Getenv("WINBASH_REAL_BASH")
	if bash == "" {
		bash = `C:\Program Files\Git\bin\bash.exe`
	}
	if _, err := os.Stat(bash); err != nil {
		t.Fatalf("找不到真 bash(%s):%v\n用 WINBASH_REAL_BASH 指定路径", bash, err)
	}

	dir := t.TempDir()
	writeGrepClassCorpus(t, dir)

	var patterns []string
	if err := json.Unmarshal([]byte(grepClassPatterns), &patterns); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	for _, p := range patterns {
		// cmd.Dir + 相对文件名:绕开 Windows 路径与 MSYS 路径的换算,少一个变量。
		cmd := exec.Command(bash, "-c", "grep -E "+shellSingleQuote(p)+" chars.txt")
		cmd.Dir = dir
		// 显式环境:滤掉 BASH_ENV / ENV / BASH_FUNC_*,并把 MSYS 与 locale 固定下来 ——
		// "哪些环境项会让参照 bash 不再可信"是一条规则,只在 testenv 里实现一份。
		cmd.Env = testenv.CleanBashEnv()
		var out, errb bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errb
		code := 0
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("跑真 bash 失败: %v", err)
			}
			code = ee.ExitCode()
		}

		rec := grepClassGolden{Pattern: p, Exit: code, Out: grepClassLines(out.String()), Stderr: errb.Len() > 0}
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}

	path := grepClassGoldenPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("已写入 %s(%d 条)", path, len(patterns))
}

func grepClassGoldenPath() string {
	return filepath.Join("testdata", "grep-e-classes.jsonl")
}

func loadGrepClassGolden(t *testing.T) []grepClassGolden {
	t.Helper()
	f, err := os.Open(grepClassGoldenPath())
	if err != nil {
		t.Fatalf("打不开 golden 表: %v", err)
	}
	defer f.Close()

	var out []grepClassGolden
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec grepClassGolden
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("golden 表里有解析不了的记录 %q: %v", line, err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func writeGrepClassCorpus(t *testing.T, dir string) {
	t.Helper()
	// 用 \n 而不是平台行尾:真 grep 看到的必须是同一份字节。
	if err := os.WriteFile(filepath.Join(dir, "chars.txt"), []byte(grepClassCorpus), 0o644); err != nil {
		t.Fatal(err)
	}
}

// grepClassLines 把输出切成行。空输出给 nil,这样与"有输出但为空行"区分得开。
func grepClassLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
