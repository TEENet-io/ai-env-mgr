package main

import "github.com/TEENet-io/airlock/internal/config"

// This file is a TEMPLATE and is committed with empty values on purpose.
//
// Fill it in locally to build, but do not commit the filled-in version: these
// are plaintext credentials, and a public commit cannot be taken back --
// deleting it later leaves the value in the history and in every mirror that
// already fetched it. If you push a real key by accident, rotate it rather
// than trying to erase it.
//
// FILL THESE IN BEFORE BUILDING agent.exe.
//
// These are the OSS settings that end up in every cloud desktop's agent.
// Use the RESTRICTED RAM user -- the one whose policy only allows reading
// policy/credentials and writing status. Never the administrator's key.
//
// ossEndpoint must be the INTERNAL one (with -internal), so the cloud
// desktops reach OSS over the VPC and the traffic is free.
//
// Leaving the key blank is fine for development: the agent then reads
// agent.config.json from its own directory instead. That is not how the image
// should be built -- `agent.exe status` prints which one is in effect.
//
// This lives in the agent's own package on purpose. Keeping it out of a
// shared package is what guarantees the administrator's read-write key is
// never compiled into a binary that ships to employees' machines. There is a
// test that builds both binaries and checks exactly that.
var (
	ossBucket          = ""
	ossEndpoint        = "oss-cn-hangzhou-internal.aliyuncs.com"
	ossAccessKeyID     = ""
	ossAccessKeySecret = ""
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
		Bucket:          ossBucket,
		Endpoint:        ossEndpoint,
		AccessKeyID:     ossAccessKeyID,
		AccessKeySecret: ossAccessKeySecret,
		IntervalMinutes: defaultSyncMinutes,
	}
}
