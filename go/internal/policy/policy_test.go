package policy

import "testing"

func TestRegistryKeyConstants(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"ChromeKey", ChromeKey, `SOFTWARE\Policies\Google\Chrome\URLBlocklist`},
		{"EdgeKey", EdgeKey, `SOFTWARE\Policies\Microsoft\Edge\URLBlocklist`},
		{"FirefoxKey", FirefoxKey, `SOFTWARE\Policies\Mozilla\Firefox\WebsiteFilter\Block`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
			}
		})
	}
}

func TestChromiumEntries(t *testing.T) {
	domains := []string{"openai.com", "chatgpt.com", "claude.ai"}
	got := ChromiumEntries(domains)

	if len(got) != len(domains) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(domains))
	}
	for i, d := range domains {
		if got[i] != d {
			t.Errorf("got[%d] = %q, want %q", i, got[i], d)
		}
	}
}

func TestChromiumEntries_EmptyAndNil(t *testing.T) {
	if got := ChromiumEntries(nil); len(got) != 0 {
		t.Errorf("ChromiumEntries(nil) = %v, want empty", got)
	}
	if got := ChromiumEntries([]string{}); len(got) != 0 {
		t.Errorf("ChromiumEntries([]string{}) = %v, want empty", got)
	}
}

func TestFirefoxEntries(t *testing.T) {
	got := FirefoxEntries([]string{"openai.com"})
	want := []string{"*://openai.com/*", "*://*.openai.com/*"}

	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d: got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFirefoxEntries_MultipleDomainsDoublesCount(t *testing.T) {
	domains := []string{"openai.com", "chatgpt.com", "claude.ai", "anthropic.com"}
	got := FirefoxEntries(domains)

	if len(got) != len(domains)*2 {
		t.Fatalf("len(got) = %d, want %d", len(got), len(domains)*2)
	}

	want := []string{
		"*://openai.com/*", "*://*.openai.com/*",
		"*://chatgpt.com/*", "*://*.chatgpt.com/*",
		"*://claude.ai/*", "*://*.claude.ai/*",
		"*://anthropic.com/*", "*://*.anthropic.com/*",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFirefoxEntries_EmptyAndNil(t *testing.T) {
	if got := FirefoxEntries(nil); len(got) != 0 {
		t.Errorf("FirefoxEntries(nil) = %v, want empty", got)
	}
	if got := FirefoxEntries([]string{}); len(got) != 0 {
		t.Errorf("FirefoxEntries([]string{}) = %v, want empty", got)
	}
}

// Off Windows there is no AppLocker to read; the stub must report "nothing
// applied" (nil, nil) rather than an error, mirroring Current().
func TestLocalAppLockerPathsOffWindows(t *testing.T) {
	got, err := LocalAppLockerPaths()
	if err != nil {
		t.Fatalf("LocalAppLockerPaths() error = %v, want nil off Windows", err)
	}
	if got != nil {
		t.Errorf("LocalAppLockerPaths() = %v, want nil off Windows", got)
	}
}

// Current() no longer reads AppLocker at all: callers that need that answer
// use LocalAppLockerPaths directly. This just locks in that Current()'s
// policy carries no AppLocker paths on this platform.
func TestCurrentDoesNotPopulateAppLockerAllowPaths(t *testing.T) {
	pol, err := Current()
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if pol.AppLockerAllowPaths != nil {
		t.Errorf("Current().AppLockerAllowPaths = %v, want nil (not populated)", pol.AppLockerAllowPaths)
	}
}
