package winmgmt

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"winbash/internal/errs"
)

// 计划里的 5 个用例照抄自 plan Task 9;后面几条是本轮补的。

const regTestKey = `HKCU\Software\winbash-selftest`

func TestRegGetDwordIsDecimalNotHex(t *testing.T) {
	// spec §6.2 的核心断言：0xa 必须变成 10
	conn := startTestPwsh(t)
	var out bytes.Buffer
	code := Reg(context.Background(), conn,
		[]string{"get", `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "CurrentMajorVersionNumber"},
		true, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if strings.Contains(out.String(), "0x") {
		t.Fatalf("hex leaked: %q", out.String())
	}
	if !strings.Contains(out.String(), "10") {
		t.Fatalf("want 10: %q", out.String())
	}
}

func TestRegGetIsTypedInJSON(t *testing.T) {
	conn := startTestPwsh(t)
	var out bytes.Buffer
	Reg(context.Background(), conn,
		[]string{"get", `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "CurrentMajorVersionNumber"},
		true, &out, io.Discard)
	var items []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &items); err != nil {
		t.Fatal(err)
	}
	if _, ok := items[0]["Data"].(float64); !ok {
		t.Fatalf("Data must be a JSON number, got %T", items[0]["Data"])
	}
}

func TestRegGetMissingValueExitsNonZero(t *testing.T) {
	conn := startTestPwsh(t)
	var errw bytes.Buffer
	code := Reg(context.Background(), conn,
		[]string{"get", `HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion`, "NoSuchValueXYZ"},
		false, io.Discard, &errw)
	if code == 0 {
		t.Fatal("expected non-zero for missing value")
	}
	if errw.Len() == 0 {
		t.Fatal("expected an error message")
	}
}

func TestRegSetThenGetRoundTripInIsolatedHive(t *testing.T) {
	// 用 HKCU 下的临时键，测完删除，避免污染
	conn := startTestPwsh(t)
	key := regTestKey
	t.Cleanup(func() { Reg(context.Background(), conn, []string{"del", key}, false, io.Discard, io.Discard) })
	if code := Reg(context.Background(), conn, []string{"set", key, "Probe", "hello"}, false, io.Discard, io.Discard); code != 0 {
		t.Fatal("set failed")
	}
	var out bytes.Buffer
	Reg(context.Background(), conn, []string{"get", key, "Probe"}, false, &out, io.Discard)
	if !strings.Contains(out.String(), "hello") {
		t.Fatalf("got %q", out.String())
	}
}

func TestRegSetWithTypeFlag(t *testing.T) {
	conn := startTestPwsh(t)
	key := regTestKey
	t.Cleanup(func() { Reg(context.Background(), conn, []string{"del", key}, false, io.Discard, io.Discard) })
	if code := Reg(context.Background(), conn, []string{"set", key, "N", "42", "--type", "REG_DWORD"}, false, io.Discard, io.Discard); code != 0 {
		t.Fatal("set with type failed")
	}
	var out bytes.Buffer
	Reg(context.Background(), conn, []string{"get", key, "N"}, true, &out, io.Discard)
	if strings.Contains(out.String(), "0x") {
		t.Fatalf("hex leaked: %q", out.String())
	}
}

// 无 name 时列出键下全部值,仍是固定三列(Name/Type/Data)。
func TestRegGetWithoutNameListsAllValues(t *testing.T) {
	conn := startTestPwsh(t)
	key := regTestKey
	t.Cleanup(func() { Reg(context.Background(), conn, []string{"del", key}, false, io.Discard, io.Discard) })
	Reg(context.Background(), conn, []string{"set", key, "A", "1"}, false, io.Discard, io.Discard)
	Reg(context.Background(), conn, []string{"set", key, "B", "2"}, false, io.Discard, io.Discard)

	var out bytes.Buffer
	code := Reg(context.Background(), conn, []string{"get", key, "--header"}, false, &out, io.Discard)
	if code != 0 {
		t.Fatal(code)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if lines[0] != "Name\tType\tData" {
		t.Fatalf("header: %q", lines[0])
	}
	if len(lines) != 3 {
		t.Fatalf("want 2 values, got %d: %q", len(lines)-1, out.String())
	}
	for _, l := range lines[1:] {
		if fields := strings.Split(l, "\t"); len(fields) != 3 {
			t.Fatalf("row is not 3 fields: %q", l)
		}
	}
}

// 文件里没有的键要明确报错,并在建议里给出两种根名写法。
func TestRegGetMissingKeyExitsNonZeroWithActionableError(t *testing.T) {
	conn := startTestPwsh(t)
	var errw bytes.Buffer
	code := Reg(context.Background(), conn, []string{"get", `HKCU\Software\definitely-not-a-key-xyz`, "v"}, false, io.Discard, &errw)
	if code == 0 {
		t.Fatal("expected non-zero")
	}
	if !strings.Contains(errw.String(), "definitely-not-a-key-xyz") {
		t.Fatalf("error should name the key: %q", errw.String())
	}
}

// 不认识的注册表根要明确报错,不能当成相对路径去猜。
func TestRegUnknownRootExitsNonZero(t *testing.T) {
	conn := startTestPwsh(t)
	var errw bytes.Buffer
	code := Reg(context.Background(), conn, []string{"get", `HKXX\Software\whatever`, "v"}, false, io.Discard, &errw)
	if code == 0 {
		t.Fatal("expected non-zero for an unknown root")
	}
	if !strings.Contains(errw.String(), "HKXX") {
		t.Fatalf("error should name the bad root: %q", errw.String())
	}
}

// 不认识的 --type 必须报错(值保证:类型决定 Data 的形态,猜错等于给了错的值)。
func TestRegSetUnknownTypeExitsUsage(t *testing.T) {
	conn := startTestPwsh(t)
	var errw bytes.Buffer
	code := Reg(context.Background(), conn, []string{"set", regTestKey, "N", "42", "--type", "REG_NOPE"}, false, io.Discard, &errw)
	if code != errs.ExitUsage {
		t.Fatalf("code=%d want %d", code, errs.ExitUsage)
	}
	if !strings.Contains(errw.String(), "REG_NOPE") {
		t.Fatalf("error must name the type: %q", errw.String())
	}
}

// keys 枚举子键:固定一列 Name。
func TestRegKeysListsSubkeys(t *testing.T) {
	conn := startTestPwsh(t)
	key := regTestKey
	t.Cleanup(func() { Reg(context.Background(), conn, []string{"del", key}, false, io.Discard, io.Discard) })
	if code := Reg(context.Background(), conn, []string{"set", key + `\sub1`, "v", "1"}, false, io.Discard, io.Discard); code != 0 {
		t.Fatal("preparing subkey failed")
	}

	var out bytes.Buffer
	code := Reg(context.Background(), conn, []string{"keys", key}, false, &out, io.Discard)
	if code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "sub1") {
		t.Fatalf("subkey not listed: %q", out.String())
	}
}

// 拼错的 -- 开关必须报「不认识的参数」,不能当键名/值名解释 —— 修前实测:
// `reg get HKCU\Environment --headr` 报「注册表值不存在: --headr」exit 1,
// `reg set … --json` 报「set 需要 <键> <值名> <数据>」,都把注意力引向了
// 错误的地方。口径对照同层 ps:ps --headr → 「不认识的参数: --headr」exit 2。
//
// conn 传 nil:拒绝发生在碰引擎之前,任何"先跑了脚本再报错"的回归都会在这
// 里 panic 而不是静默写注册表。
func TestRegMisspelledFlagIsRejectedNotInterpreted(t *testing.T) {
	cases := []struct {
		name string
		argv []string
	}{
		{"get", []string{"get", `HKCU\Environment`, "--headr"}},
		{"set", []string{"set", `HKCU\Software\winbash-selftest`, "N", "42", "--json"}},
		{"del", []string{"del", `HKCU\Software\winbash-selftest`, "--headr"}},
		{"keys", []string{"keys", `HKCU\Environment`, "--headr"}},
		// 裸 -- 分隔符本层不支持(同 ps),同样按不认识的参数拒绝,不当键名。
		{"get-bare-dashdash", []string{"get", `HKCU\Environment`, "--"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errw bytes.Buffer
			code := Reg(context.Background(), nil, tc.argv, false, io.Discard, &errw)
			if code != errs.ExitUsage {
				t.Fatalf("%s: code=%d want %d", tc.name, code, errs.ExitUsage)
			}
			msg := errw.String()
			if !strings.Contains(msg, "不认识的参数") {
				t.Fatalf("%s: must say 不认识的参数, got %q", tc.name, msg)
			}
			if !strings.Contains(msg, tc.argv[len(tc.argv)-1]) {
				t.Fatalf("%s: error must name the original arg: %q", tc.name, msg)
			}
			for _, misleading := range []string{"注册表值不存在", "需要 <键>", "需要一个键"} {
				if strings.Contains(msg, misleading) {
					t.Fatalf("%s: misleading message leaked: %q", tc.name, msg)
				}
			}
			// remedy 要列出该子命令支持的开关,而不是只甩用法。
			if !strings.Contains(msg, "开关") {
				t.Fatalf("%s: remedy should name the supported flags: %q", tc.name, msg)
			}
		})
	}
}

// set 的带值写法 --type=REG_DWORD 与空格写法等效(白名单按前缀放行它)。
func TestRegSetWithTypeEqualsForm(t *testing.T) {
	conn := startTestPwsh(t)
	key := regTestKey
	t.Cleanup(func() { Reg(context.Background(), conn, []string{"del", key}, false, io.Discard, io.Discard) })
	if code := Reg(context.Background(), conn, []string{"set", key, "N", "42", "--type=REG_DWORD"}, false, io.Discard, io.Discard); code != 0 {
		t.Fatalf("set with --type= failed: %d", code)
	}
	var out bytes.Buffer
	Reg(context.Background(), conn, []string{"get", key, "N"}, false, &out, io.Discard)
	if strings.Contains(out.String(), "0x") {
		t.Fatalf("hex leaked: %q", out.String())
	}
	if !strings.Contains(out.String(), "42") {
		t.Fatalf("want 42: %q", out.String())
	}
}

// set 必须回读出真实值:与其说「命令成功了」,不如给出结果本身。
func TestRegSetReportsTheStoredValue(t *testing.T) {
	conn := startTestPwsh(t)
	t.Cleanup(func() { Reg(context.Background(), conn, []string{"del", regTestKey}, false, io.Discard, io.Discard) })

	var out bytes.Buffer
	if code := Reg(context.Background(), conn, []string{"set", regTestKey, "Probe", "hello"}, false, &out, io.Discard); code != 0 {
		t.Fatal(code)
	}
	if fields := strings.Split(firstLine(out.String()), "\t"); len(fields) != 3 || fields[0] != "Probe" || fields[2] != "hello" {
		t.Fatalf("set should report the stored value: %q", out.String())
	}
}
