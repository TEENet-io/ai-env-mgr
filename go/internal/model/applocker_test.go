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

// The one-directional prefix test this replaces refused C:\Windows\Temp\* but
// accepted its parent C:\Windows\*, which adds an Everyone/Allow rule with no
// <Exceptions> block and hands wscript.exe, cmd.exe and powershell.exe back to
// every standard user -- the exact bypass the image's exception list closes.
// C:\ProgramData\* is the drive-letter spelling of %PROGRAMDATA%\*, which was
// already refused explicitly.
func TestValidateAppLockerPathRefusesTreesContainingAForbiddenLocation(t *testing.T) {
	cases := []struct {
		path string
		want bool // true = must be rejected
	}{
		{`C:\Windows\*`, true},
		{`C:\windows\system32\*`, true},
		{`%OSDRIVE%\Windows\*`, true},
		{`%WINDIR%\*`, true},
		{`C:\ProgramData\*`, true},
		{`%OSDRIVE%\ProgramData\*`, true},
		{`C:\Users/alice\*`, true},
		{`C:\WINDOWS\System32\*`, true},
		{`%OSDRIVE%\*`, true},
		{"C:\\a\nb\\*", true},
		{"C:\\a\tb\\*", true},

		// Spellings the Win32 path parser folds away: a doubled separator, a
		// trailing dot or space in a component. If AppLocker normalises a rule
		// path the way the file system does -- and that is the way to bet --
		// each of these is an Everyone/Allow rule over the Windows tree with
		// no exceptions on it.
		{`C:\\Windows\*`, true},
		{`C:\Windows.\*`, true},
		{`C:\Windows \*`, true},
		{`C:\ProgramData\\*`, true},
		{`C:\\Users\\alice\\*`, true},
		{`%OSDRIVE%\\Windows\*`, true},

		// The forbidden locations are names, not volumes: redirected profiles
		// or a second system volume make these live.
		{`D:\Windows\*`, true},
		{`E:\Users\*`, true},
		{`D:\ProgramData\*`, true},

		{`C:\tools\Codex\*`, false},
		{`D:\Apps\Foo\*`, false},
		{`%PROGRAMFILES%\Vendor\*`, false},
		{`%PROGRAMDATA%\Vendor\App\*`, false},
		{`%OSDRIVE%\tools\Codex\*`, false},

		// ...and the drive-agnostic test must not over-reject: these are
		// prefix tests on \-terminated segments, so a directory that merely
		// starts with a forbidden name is fine.
		{`D:\Users2\*`, false},
		{`D:\Windows Apps\*`, false},
		{`C:\WindowsTools\*`, false},
	}
	for _, c := range cases {
		err := ValidateAppLockerPath(c.path)
		if c.want && err == nil {
			t.Errorf("%q must be rejected, got nil", c.path)
		}
		if !c.want && err != nil {
			t.Errorf("%q must be accepted, got %v", c.path, err)
		}
	}
}

func TestNormalizeAppLockerPaths(t *testing.T) {
	got := NormalizeAppLockerPaths([]string{` C:\tools\Codex\* `, `c:\TOOLS\codex\*`, ``, `C:\Apps\Foo\*`})
	if len(got) != 2 || got[0] != `C:\Apps\Foo\*` || got[1] != `C:\tools\Codex\*` {
		t.Errorf("got %q", got)
	}
}
