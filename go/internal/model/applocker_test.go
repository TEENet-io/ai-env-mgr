package model

import "testing"

func TestValidateAppLockerPathAcceptsMachineLevelDirs(t *testing.T) {
	for _, p := range []string{
		`C:\tools\Codex\*`, `D:\Apps\Foo\*`, `%PROGRAMFILES%\Vendor\*`,
		`%PROGRAMDATA%\Vendor\App\*`, `%OSDRIVE%\tools\Codex\*`,
	} {
		if err := ValidateAppLockerPath(p); err != nil {
			t.Errorf("%q should be accepted: %v", p, err)
		}
	}
}

func TestValidateAppLockerPathRejectsUserWritableAndMalformed(t *testing.T) {
	for _, p := range []string{
		``, `C:\*`, `c:\users\alice\*`, `C:\Users\*`, `%USERPROFILE%\x\*`, `%LOCALAPPDATA%\Codex\*`,
		`%APPDATA%\x\*`, `%TEMP%\*`, `%TMP%\x\*`, `%PUBLIC%\x\*`, `C:\Windows\Temp\*`,
		`%PROGRAMDATA%\*`, `C:\tools\Codex`, `C:\tools\*\bin\*`, `C:\tools\..\Windows\*`,
		`\\server\share\*`, `tools\Codex\*`, `%OSDRIVE%\Users\bob\*`,
	} {
		if err := ValidateAppLockerPath(p); err == nil {
			t.Errorf("%q should be rejected", p)
		}
	}
	long := `C:\` + string(make([]byte, 200)) + `\*`
	if err := ValidateAppLockerPath(long); err == nil {
		t.Error("over-long path should be rejected")
	}
}

func TestNormalizeAppLockerPaths(t *testing.T) {
	got := NormalizeAppLockerPaths([]string{` C:\tools\Codex\* `, `c:\TOOLS\codex\*`, ``, `C:\Apps\Foo\*`})
	if len(got) != 2 || got[0] != `C:\Apps\Foo\*` || got[1] != `C:\tools\Codex\*` {
		t.Errorf("got %q", got)
	}
}
