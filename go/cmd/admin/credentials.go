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

// Cloud desktop lookup (WuYing / ECD), used by the web console to tell a
// hibernating machine from one whose agent has died. WuYing suspends at the
// hypervisor, so the guest never sees a power event and the agent cannot
// report it -- the platform is the only source.
//
// The region is not a secret and is baked in. The key pair follows the same
// rule as the OSS one above: BLANK here, injected with -ldflags at build time,
// never committed. Use a RAM user with ONLY AliyunECDReadOnlyAccess and no OSS
// permissions at all -- it is read-only and cannot change anything, so a leak
// costs a list of desktops rather than control of the fleet.
//
// Deliberately NOT the agent's key. That one is baked into agent.exe on every
// employee desktop in plaintext, and widening it to cover ECD would let any
// one of those machines enumerate every desktop in the organisation, with the
// employee assigned to each.
var (
	ecdRegion          = "ap-southeast-1"
	ecdAccessKeyID     = ""
	ecdAccessKeySecret = ""
)

func builtIn() config.Config {
	return config.Config{
		Bucket:          ossBucket,
		Endpoint:        ossEndpoint,
		AccessKeyID:     ossAccessKeyID,
		AccessKeySecret: ossAccessKeySecret,
	}
}
