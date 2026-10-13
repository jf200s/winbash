package errs_test

import (
	"strings"
	"testing"

	"winbash/internal/errs"
)

func TestExitCodesFollowShellConvention(t *testing.T) {
	if errs.ExitOK != 0 || errs.ExitError != 1 || errs.ExitUsage != 2 || errs.ExitNoExec != 126 || errs.ExitNotFound != 127 {
		t.Fatalf("exit codes drifted: %d %d %d %d %d",
			errs.ExitOK, errs.ExitError, errs.ExitUsage, errs.ExitNoExec, errs.ExitNotFound)
	}
}

func TestErrorfIsThreeLines(t *testing.T) {
	got := errs.Errorf("reg get", "参数 '/v' 不被识别", `直接用: winbash -c 'reg get HKCU\Environment PATH'`)
	if n := strings.Count(got, "\n"); n != 2 {
		t.Fatalf("want 3 lines, got %d newlines: %q", n, got)
	}
}

func TestTranslateKnownTrapRecognizesBashAsWSL(t *testing.T) {
	_, remedy, ok := errs.TranslateKnownTrap("bash", []string{"x.sh"}, "execvpe(/bin/bash) failed")
	if !ok || !strings.Contains(remedy, `C:\Program Files\Git\bin\bash.exe`) {
		t.Fatalf("ok=%v remedy=%q", ok, remedy)
	}
}

func TestTranslateKnownTrapIgnoresOrdinaryFailures(t *testing.T) {
	if _, _, ok := errs.TranslateKnownTrap("ls", []string{"nope"}, "no such file"); ok {
		t.Fatal("ordinary failure must not be reported as a known trap")
	}
}
