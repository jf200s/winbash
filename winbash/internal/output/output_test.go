package output

import "testing"

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
