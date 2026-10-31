package output

import (
	"encoding/json"
	"testing"
)

func TestFormatBytesIsPlainNumberByDefault(t *testing.T) {
	if got := FormatBytes(86488064, false); got != "86488064" {
		t.Fatalf("got %q", got)
	}
}

// StripANSI 是链路里唯一的色码兜底(sanitizeCell / cellText 每个单元格都过它)。
// 它必须是幂等的、并且对没有色码的输入原样返回 —— 否则会改坏已经正确的值。
func TestStripANSIRemovesColorCodes(t *testing.T) {
	if got := StripANSI("\x1b[32;1mName\x1b[0m"); got != "Name" {
		t.Fatalf("got %q", got)
	}
	if got := StripANSI("Running"); got != "Running" {
		t.Fatalf("无色码的输入被改动了: %q", got)
	}
}

func TestTSVHasFixedColumnsAndOptionalHeader(t *testing.T) {
	rows := [][]string{{"Spooler", "Running", "Automatic"}}
	if got := TSV([]string{"Name", "Status", "StartType"}, rows, false); got != "Spooler\tRunning\tAutomatic\n" {
		t.Fatalf("no header: %q", got)
	}
	want := "Name\tStatus\tStartType\nSpooler\tRunning\tAutomatic\n"
	if got := TSV([]string{"Name", "Status", "StartType"}, rows, true); got != want {
		t.Fatalf("with header: %q", got)
	}
}

func TestJSONArrayIsTypedArray(t *testing.T) {
	got, err := JSONArray([]any{map[string]any{"Name": "Spooler", "MemoryBytes": 86488064}})
	if err != nil {
		t.Fatal(err)
	}
	if got != `[{"MemoryBytes":86488064,"Name":"Spooler"}]` {
		t.Fatalf("got %s", got)
	}
}

type unserializable struct{ C chan int }

func TestJSONArrayDegradesUnserializableValueInsteadOfFailing(t *testing.T) {
	// Review Focus 第 3 条：不可序列化的值必须有确定的表示，不得让整个输出失败
	got, err := JSONArray([]any{unserializable{}})
	if err != nil {
		t.Fatalf("must degrade, not fail: %v", err)
	}
	if got == "" || got[0] != '[' {
		t.Fatalf("got %q", got)
	}
}

// human 模式是给人看的另一套合同:单位阶梯 K→M→G→T→P、固定一位小数、
// 不足 1K 带 B 后缀。超过 P 没有更大的单位,钳在最大档 —— "1024.0P"
// 仍是个可解析的数,总比溢出或换形状强。
func TestFormatBytesHumanUsesUnitLadder(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1023, "1023B"},
		{1024, "1.0K"},
		{1536, "1.5K"},
		{1 << 20, "1.0M"},
		{86488064, "82.5M"}, // 本仓库的常驻数字:86,488 K
		{1 << 30, "1.0G"},
		{1 << 40, "1.0T"},
		{1 << 50, "1.0P"},
		{1 << 60, "1024.0P"}, // 无 E 档,钳在 P
	}
	for _, c := range cases {
		if got := FormatBytes(c.n, true); got != c.want {
			t.Errorf("FormatBytes(%d, true) = %q, want %q", c.n, got, c.want)
		}
	}
}

// 空输入必须还是数组形状("[]"),不能退化成 "null" —— AI 的解析器依赖形状稳定。
func TestJSONArrayEmptyInputIsStillAnArray(t *testing.T) {
	got, err := JSONArray(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "[]" {
		t.Fatalf("got %q, want []", got)
	}
}

// 一个坏元素只降级它自己:条数与顺序都不能变,前后好元素原样保留。
// 这是逐个 Marshal(而不是 Marshal 整个切片)买到的性质,值得钉住。
func TestJSONArrayBadElementOnlyDegradesItself(t *testing.T) {
	got, err := JSONArray([]any{
		map[string]any{"Id": 1},
		unserializable{},
		map[string]any{"Id": 2},
	})
	if err != nil {
		t.Fatalf("must degrade the bad element, not fail: %v", err)
	}
	var arr []any
	if err := json.Unmarshal([]byte(got), &arr); err != nil {
		t.Fatalf("out is not a JSON array: %v (%q)", err, got)
	}
	if len(arr) != 3 {
		t.Fatalf("want 3 elements, got %d: %s", len(arr), got)
	}
	first, ok := arr[0].(map[string]any)
	if !ok || first["Id"] != float64(1) {
		t.Fatalf("good element before the bad one was altered: %s", got)
	}
	if _, ok := arr[1].(string); !ok {
		t.Fatalf("bad element must degrade to its string form, got %T: %s", arr[1], got)
	}
	last, ok := arr[2].(map[string]any)
	if !ok || last["Id"] != float64(2) {
		t.Fatalf("good element after the bad one was altered: %s", got)
	}
}
