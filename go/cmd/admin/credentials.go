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
// ossEndpoint is the PUBLIC one (no -internal), unless the administrator's
// machine sits in the same VPC as the bucket.
//
// admin.exe never leaves the administrator's machine, so a config file is
// perfectly reasonable here and is easier to change. Keeping these in the
// admin's own package is what guarantees the read-write key is never
// compiled into agent.exe.
var (
	ossBucket          = ""
	ossEndpoint        = "oss-cn-hangzhou.aliyuncs.com"
	ossAccessKeyID     = ""
	ossAccessKeySecret = ""
)

func builtIn() config.Config {
	return config.Config{
		Bucket:          ossBucket,
		Endpoint:        ossEndpoint,
		AccessKeyID:     ossAccessKeyID,
		AccessKeySecret: ossAccessKeySecret,
	}
}
