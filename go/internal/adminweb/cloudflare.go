package adminweb

import "html/template"

// noEmailScan renders a value with CloudFlare's email obfuscation turned off
// around it.
//
// CloudFlare rewrites anything that looks like an email address into
// "[email protected]" and injects a script to restore it in the browser. This
// console's CSP allows no script at all, so that script never runs and the
// address stays unreadable -- the roster's account column would show nothing
// useful. The <!--email_off--> markers tell CloudFlare to leave the region
// alone.
//
// html/template strips HTML comments, so the markers cannot simply be written
// into a template; they have to be emitted as trusted HTML. The escaping is
// done here rather than left to the caller, because the value is roster data
// and a caller that forgot would be injecting markup.
func noEmailScan(value string) template.HTML {
	if value == "" {
		return ""
	}
	return template.HTML("<!--email_off-->" + template.HTMLEscapeString(value) + "<!--email_on-->")
}
