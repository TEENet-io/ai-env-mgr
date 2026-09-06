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
// FILL THESE IN BEFORE BUILDING admin.exe, or leave them blank and use
// admin.config.json instead.
//
// These are the OSS settings for the administrator's own machine. Use the
// READ-WRITE RAM user -- admin.exe has to publish policy and credentials, so
// the restricted agent key will not do.
//
// bucket and endpoint are the deployment's fixed OSS location. They are baked
// in so admin only ever asks for the AccessKey. Neither is a secret -- a bucket
// name and a region -- so committing them is fine. endpoint is the PUBLIC one
// (no -internal), unless the administrator's machine sits in the same VPC as
// the bucket.
//
// The AccessKey pair stays BLANK and is never committed: it is the read-write
// key, prompted at runtime and never written to disk. -ldflags -X can still
// override any of these at build time.
//
// Keeping these in the admin's own package is what guarantees the read-write
// key is never compiled into agent.exe.
var (
	ossBucket          = "ai-collect-sg"
	ossEndpoint        = "oss-ap-southeast-1.aliyuncs.com"
	ossAccessKeyID     = ""
	ossAccessKeySecret = ""
)

// Cloud desktop lookup (WuYing / ECD), used by the console to tell a
// hibernating machine from one whose agent has died. WuYing suspends at the
// hypervisor, so the guest never sees a power event and the agent cannot
// report it -- the platform is the only source.
//
// Only the region lives here, and it is not a secret. The lookup runs with the
// credentials the administrator signed in with, so no key is stored anywhere
// and none ships inside the binary. That costs nothing: the lookup exists to
// render a page, so it is only ever wanted while somebody is signed in.
//
// It does mean the administrator's own RAM user needs AliyunECDReadOnlyAccess
// alongside its OSS permissions. Widening that key is safe in a way widening
// the agent's is not: this one is typed at runtime and never distributed,
// while the agent's sits in plaintext on every employee desktop.
var ecdRegion = "ap-southeast-1"

// The model gateway (LiteLLM) the console issues employee tokens against.
//
// Baked in for the same reason bucket and endpoint are: it is the
// deployment's fixed address, not something an operator should be able to
// retarget from the command line. Pointing the console at another gateway
// would mean issuing tokens on one and delivering configuration for another,
// and the mismatch would only surface as employees whose Codex cannot reach
// anything.
//
// Not a secret -- a hostname. The management key that goes with it is NOT
// here: it comes from AIENVMGR_GATEWAY_ADMIN_KEY at startup and lives only in
// process memory, so it is never compiled into a binary that gets copied
// around. Empty disables the gateway page entirely.
//
// -ldflags -X can still override this at build time.
var gatewayURL = "https://litellm.teenet.app"

func builtIn() config.Config {
	return config.Config{
		Bucket:          ossBucket,
		Endpoint:        ossEndpoint,
		AccessKeyID:     ossAccessKeyID,
		AccessKeySecret: ossAccessKeySecret,
	}
}
