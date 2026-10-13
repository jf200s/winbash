package output

import "testing"

func TestFormatDWORDIsDecimalNotHex(t *testing.T) {
	if got := FormatDWORD(10); got != "10" {
		t.Fatalf("got %q want \"10\"", got)
	}
}

func TestFormatBytesIsPlainNumberByDefault(t *testing.T) {
	if got := FormatBytes(86488064, false); got != "86488064" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatServiceStateMapsConstantCode(t *testing.T) {
	got, ok := FormatServiceState(1)
	if !ok || got != "Stopped" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
}

func TestFormatServiceStateUnknownValueIsNotGuessed(t *testing.T) {
	// 不认识的状态码不许猜 —— 返回数字本身并标记 ok=false
	got, ok := FormatServiceState(99)
	if ok || got != "99" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
}

func TestStripANSIRemovesColorCodes(t *testing.T) {
	if got := StripANSI("\x1b[32;1mName\x1b[0m"); got != "Name" {
		t.Fatalf("got %q", got)
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
