package main

import "github.com/TEENet-io/ai-env-mgr/internal/config"

// This file is a TEMPLATE and is committed with empty values on purpose.
//
// Fill it in locally to build, but do not commit the filled-in version: these
// are plaintext credentials, and a public commit cannot be taken back --
// deleting it later leaves the value in the history and in every mirror that
// already fetched it. If you push a real key by accident, rotate it rather
// than trying to erase it.
//
// The agent carries no OSS bucket credentials. It enrols with the console and
// uses the device token plus short-lived signed links for objects and uploads.
var (
	// consoleURL is the console the agent enrols with and talks to directly.
	// Not a secret: it is the public address. Empty means the bucket is the
	// only channel, as before 1.3.0.
	consoleURL = ""
)

// defaultSyncMinutes is how often the agent syncs when the policy in OSS does
// not say otherwise. Not a credential -- leave it alone unless you have a
// reason. The administrator can change the live value at any time with
// `admin.exe set-interval`, which every machine picks up on its next cycle.
const defaultSyncMinutes = 30

// builtIn is what the rest of the agent reads. It is a function rather than a
// package-level value so a test can override the vars above through the
// linker and still see the result.
func builtIn() config.Config {
	return config.Config{
		IntervalMinutes: defaultSyncMinutes,
		ConsoleURL:      consoleURL,
	}
}
