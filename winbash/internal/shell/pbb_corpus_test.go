package shell

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

// 本文件的语料取自 pure-bash-bible(https://github.com/dylanaraps/pure-bash-bible)。
//
// 选它的理由:那本"技巧集"里全是**参数扩展、数组、IFS、间接引用**这些 ① 层自己实现
// (mvdan/sh)的部分 —— 恰好是 winbash 与真 Git Bash 最可能产生差异的地带,也是 AI
// 写给它的脚本里出现频率最高的一类写法。用它的例子当语料,等于拿一份"真实世界里的
// bash 用法样本"来压 ① 层,比我们自己想例子覆盖面宽得多。
//
// 期望值由真 Git Bash 生成(见 TestUpdatePbbCorpus),不手写:手写的期望值只能覆盖
// 作者想到的形状,而"跑真 bash 取一遍"能覆盖作者没想到的。
//
// 与 corpus_test.go 里那份语料的分工:那份钉的是**已确认的差异**(带 known 说明),
// 这份钉的是**大量正常用法**——它大多数时候应该全绿,作用是"别把已经对的东西改坏"。

type pbbCase struct {
	name string
	// section 是 pure-bash-bible 里的来源小节,便于回查原始用法。
	section string
	script  string
	// known 非空 = 这是**已知**与真 bash 不一致的地方,值为原因。
	// 这种条目会把"差异仍然存在"钉住 —— 哪天它自己变一致了,测试会失败提醒删掉。
	known string
}

var pbbCases = []pbbCase{
	// ---- STRINGS:trim / strip ----
	{"trim 两端空白", "STRINGS / Trim leading and trailing white-space", `s="   example   "; printf '[%s]\n' "${s#"${s%%[![:space:]]*}"}"`, ""},
	{"trim 全部并压缩", "STRINGS / Trim all white-space and truncate spaces", `s="  a   b  c  "; read -ra p <<< "$s"; printf '[%s]\n' "${p[*]}"`, ""},
	{"strip_all 去掉全部", "STRINGS / Strip all instances of pattern", `s="a-b-c-d"; printf '%s\n' "${s//-/}"`, ""},
	{"strip 只去第一处", "STRINGS / Strip first occurrence of pattern", `s="a-b-c-d"; printf '%s\n' "${s/-/}"`, ""},
	{"lstrip 去开头", "STRINGS / Strip pattern from start", `s="a-b-c"; printf '%s\n' "${s#a-}"`, ""},
	{"rstrip 去结尾", "STRINGS / Strip pattern from end", `s="a-b-c"; printf '%s\n' "${s%-c}"`, ""},
	{"去引号", "STRINGS / Trim quotes from a string", `s="\"quoted\""; printf '%s\n' "${s//\"/}"`, ""},

	// ---- STRINGS:大小写 ----
	{"小写", "STRINGS / Change a string to lowercase", `s="HeLLo"; printf '%s\n' "${s,,}"`, ""},
	{"大写", "STRINGS / Change a string to uppercase", `s="HeLLo"; printf '%s\n' "${s^^}"`, ""},
	{"翻转大小写", "STRINGS / Reverse a string case", `s="HeLLo"; printf '%s\n' "${s~~}"`, "mvdan/sh 不支持 `${v~~}`;我们**明确报错**(exit 2),而不是静默原样输出 —— 与项目底线一致,故只记账"},
	{"首字母大写", "STRINGS / (参数扩展变体)", `s="hello world"; printf '%s\n' "${s^}"`, ""},

	// ---- STRINGS:切分与查找 ----
	{"按单字符分开", "STRINGS / Split a string on a delimiter", `s="a,b,c"; IFS=, read -ra p <<< "$s"; printf '%s|%s\n' "${#p[@]}" "${p[1]}"`, ""},
	{"按多字符分开", "STRINGS / Split (multi char delimiters)", `s="aBCbBCc"; IFS="BC" read -ra p <<< "$s"; printf '%s|%s|%s\n' "${p[0]}" "${p[1]}" "${p[2]}"`, "IFS 给多字符时,winbash 把**整个串**当分隔符,真 bash 只取**第一个字符**(IFS=BC 等价 IFS=B)。mvdan/sh 的缺口,而且**静默**给出不同结果 —— 光看输出看不出这是错的"},
	{"子串长度与切片", "STRINGS / (字符计数)", `s="abcdefgh"; printf '%s|%s|%s\n' "${#s}" "${s:2:3}" "${s: -2}"`, ""},
	{"包含子串", "STRINGS / Check if string contains a sub-string", `s="hello world"; if [[ $s == *"wor"* ]]; then echo yes; else echo no; fi`, ""},
	{"按正则匹配抓组", "STRINGS / Use regex on a string", `s="2026-10-15"; if [[ $s =~ ^([0-9]{4})-([0-9]{2}) ]]; then printf '%s|%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"; fi`, ""},

	// ---- STRINGS:替换 ----
	{"替换全部", "STRINGS / (参数扩展替换)", `v="/p/a/b"; printf '%s\n' "${v//\//-}"`, ""},
	{"只替换开头", "STRINGS / (参数扩展替换)", `v="aaa"; printf '%s\n' "${v/#a/X}"`, "mvdan/sh 不支持 `${v/#pat/rep}`(报 not supported);我们把它变成**明确报错**(exit 2),而不是静默原样输出"},
	{"默认值空值", "STRINGS / (参数扩展默认值)", `u=; d="set"; printf '[%s][%s][%s]\n' "${u:-fallback}" "${d:-fallback}" "${u:+alt}"`, ""},
	{"赋值默认值", "STRINGS / (参数扩展:=)", `unset u; printf '[%s][%s]\n' "${u:=filled}" "$u"`, ""},

	// ---- STRINGS:长度与截断 ----
	{"取前 N / 去前 N", "STRINGS / (前缀修剪)", `s="abcdef"; printf '%s|%s\n' "${s:0:3}" "${s:3}"`, ""},
	{"printf 重复字符", "OTHERS / (printf 技巧)", `printf '=%.0s' 1 2 3 4 5; echo`, "mvdan/sh 的内建 printf 不认 `%.0s`(报 invalid format char),而且**退出码仍是 0** —— 这是这批语料里唯一「打了 stderr 但退出码不非零」的一条,值得单独盯着"},

	// ---- FILE PATHS ----
	{"去掉目录名", "FILE PATHS / Get the directory name of a file path", `p="/etc/x/y.conf"; printf '%s\n' "${p%/*}"`, ""},
	{"取文件名", "FILE PATHS / Get the basename of a file path", `p="/etc/x/y.conf"; printf '%s\n' "${p##*/}"`, ""},
	{"取扩展名", "FILE PATHS / Get the extension of a file", `p="/etc/x/y.tar.gz"; printf '%s\n' "${p##*.}"`, ""},
	{"去掉扩展名", "FILE PATHS / Get the filename without extension", `p="/etc/x/y.tar.gz"; printf '%s\n' "${p%%.*}"`, ""},

	// ---- ARRAYS ----
	{"数组元素与长度", "ARRAYS / (基础)", `a=(p q r); printf '%s|%s|%s\n' "${a[0]}" "${a[-1]}" "${#a[@]}"`, ""},
	{"数组遍历", "ARRAYS / Loop over an array", `a=(x y z); for e in "${a[@]}"; do printf '<%s>' "$e"; done; echo`, ""},
	{"数组切片", "ARRAYS / (数组切片)", `a=(a b c d e); printf '%s\n' "${a[@]:1:3}"`, ""},
	{"数组反转", "ARRAYS / Reverse an array", `a=(a b c); for ((i=${#a[@]}-1; i>=0; i--)); do printf '%s' "${a[i]}"; done; echo`, ""},
	{"数组去重", "ARRAYS / Remove duplicate array elements", `a=(a b a c b); declare -A seen; o=(); for e in "${a[@]}"; do [[ -z ${seen[$e]} ]] && o+=("$e"); seen[$e]=1; done; printf '%s\n' "${o[*]}"`, ""},
	{"关联数组键", "ARRAYS / (关联数组)", `declare -A m=([k1]=v1 [k2]=v2); printf '%s\n' "${#m[@]}"`, "README 的已知差异里已记:${#关联数组[@]} 恒返回 1(mvdan/sh 的缺口) —— 键列表与取值都是对的,只有计数不对"},

	// ---- VARIABLES ----
	{"间接引用", "VARIABLES / Indirect expansion", `name=target; target="indirect-ok"; printf '%s\n' "${!name}"`, ""},
	{"变量是否存在", "VARIABLES / (判断已定义)", `defined=1; printf '%s|%s\n' "${defined+yes}" "${undefined+yes}"`, ""},

	// ---- OTHERS / IFS ----
	{"IFS 影响 $*", "OTHERS / (IFS 与位置参数)", `set -- a b c; IFS=-; printf '%s\n' "$*"`, ""},
	{"here-string 与 read", "OTHERS / (here-string)", `read -r a b <<< "one two three"; printf '%s|%s\n' "$a" "$b"`, ""},
	{"命令替换嵌套", "OTHERS / (命令替换)", `printf '%s\n' "$(printf '%s' "$(echo deep)")"`, ""},
} // pbbCases

var updatePbb = flag.Bool("update-pbb-corpus", false,
	"用真 Git Bash 重新生成 testdata/pbb-corpus.jsonl")

type pbbGolden struct {
	Name string   `json:"name"`
	Out  []string `json:"out"`
	Exit int      `json:"exit"`
	// ErrNonEmpty 记"真 bash 有没有往 stderr 写东西"(不记内容:两侧措辞本来就不同,
	// winbash 用中文三行式)。这是判据的一部分 —— 回应 §四 特别点出 `printf '%.0s'`
	// 那条"打了 stderr 但退出码仍是 0"最需要盯住,而只看 exit/stdout 正好漏掉它。
	ErrNonEmpty bool   `json:"err_non_empty,omitempty"`
	Known       string `json:"known,omitempty"`
	Source      string `json:"source,omitempty"`
}

// TestPbbCorpusMatchesRealBash 拿 pure-bash-bible 的用法压 ① 层,期望值来自真 Git Bash。
func TestPbbCorpusMatchesRealBash(t *testing.T) {
	golden := loadPbbGolden(t)
	byName := make(map[string]pbbGolden, len(golden))
	for _, g := range golden {
		byName[g.Name] = g
	}

	// 本测试**不**需要真 Git Bash:它比的是已提交的 golden 表,真 bash 只在
	// TestUpdatePbbCorpus 里才要。早先这里调了 findRealBash,于是没有 Git Bash 的
	// 机器/CI 上 37 条子用例会**整条 skip** —— 回归网静默消失,但表面上还是"绿"的。
	for _, c := range pbbCases {
		t.Run(c.name, func(t *testing.T) {
			want, ok := byName[c.name]
			if !ok {
				t.Fatalf("golden 表里没有 %q —— 跑一遍 -update-pbb-corpus", c.name)
			}

			s := NewSession()
			defer s.Close()
			out, exit, errs := s.Run(context.Background(), c.script, false)
			gotOut := pbbLines(out)

			same := exit == want.Exit && slices.Equal(gotOut, want.Out) &&
				(len(errs) > 0) == want.ErrNonEmpty

			if c.known != "" {
				// 已知差异:断言"差异**仍然**存在"——哪天它自己一致了就该删掉这条,
				// 否则它会变成"不做任何检查的空壳"(空壳的 known 比没有更坏)。
				if same {
					t.Errorf("这条 known 差异已经不存在了(两边一致),请把它删掉:%s", c.known)
				}
				t.Logf("已知差异(仍存在):%s", c.known)
				return
			}

			if !same {
				t.Errorf("与真 bash 不一致\n  script : %s\n  winbash: exit=%d out=%q\n  真bash : exit=%d out=%q\n  来源   : %s",
					c.script, exit, gotOut, want.Exit, want.Out, c.section)
				if len(errs) > 0 {
					t.Errorf("winbash stderr: %s", strings.Join(errs, " / "))
				}
			}
		})
	}
}

// TestUpdatePbbCorpus 用真 Git Bash 重新生成 golden 表。默认跳过。
func TestUpdatePbbCorpus(t *testing.T) {
	if !*updatePbb {
		t.Skip("只在 -update-pbb-corpus 时重新生成")
	}
	bash := findRealBash(t)
	if bash == "" {
		t.Fatal("找不到真 Git Bash")
	}

	var buf bytes.Buffer
	for _, c := range pbbCases {
		cmd := exec.Command(bash, "-c", c.script)
		cmd.Env = testenv.WithPosixPath(testenv.CleanBashEnv(), filepath.Dir(bash))
		cmd.Stdin = strings.NewReader("")
		var out, errb bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errb
		code := 0
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("%s: 跑真 bash 失败: %v", c.name, err)
			}
			code = ee.ExitCode()
		}

		rec := pbbGolden{
			Name:        c.name,
			Out:         pbbLines(out.String()),
			Exit:        code,
			ErrNonEmpty: errb.Len() > 0,
			Known:       c.known,
			Source:      c.section,
		}
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}

	path := pbbGoldenPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("已写入 %s(%d 条)", path, len(pbbCases))
}

func pbbGoldenPath() string { return filepath.Join("testdata", "pbb-corpus.jsonl") }

func loadPbbGolden(t *testing.T) []pbbGolden {
	t.Helper()
	f, err := os.Open(pbbGoldenPath())
	if err != nil {
		t.Fatalf("打不开 golden 表: %v(跑一遍 -update-pbb-corpus)", err)
	}
	defer f.Close()

	var out []pbbGolden
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec pbbGolden
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

func pbbLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
