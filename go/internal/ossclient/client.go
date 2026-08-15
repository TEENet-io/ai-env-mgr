// Package ossclient wraps the parts of the Aliyun OSS SDK this project uses.
//
// Everything this project stores lives under a single top-level directory in
// the bucket, so every helper here works in terms of object keys like
// "agent_workdir/work1/credentials.zip".
//
// This file and OSS布局.md one level up are the two authoritative descriptions
// of that layout. Change one and you must change the other.
package ossclient

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

// Client is a thin wrapper around a single OSS bucket.
type Client struct {
	bucket *oss.Bucket
}

// withHTTPS makes a scheme-less endpoint use TLS.
//
// The OSS SDK defaults a bare host to http, which would send credentials.zip
// -- the employee's live AI tokens -- across the wire in the clear, and would
// sign download links as http URLs. Being on a VPC is not a reason to skip
// TLS. An explicit scheme is honoured either way, so http:// remains
// available for anyone who genuinely needs it.
func withHTTPS(endpoint string) string {
	if strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://") {
		return endpoint
	}
	return "https://" + endpoint
}

// New builds a client. Cloud desktops pass the internal endpoint and a
// restricted key; the admin machine passes the public endpoint and a
// read-write key.
func New(endpoint, bucket, accessKeyID, accessKeySecret string) (*Client, error) {
	switch {
	case endpoint == "":
		return nil, fmt.Errorf("endpoint is required")
	case bucket == "":
		return nil, fmt.Errorf("bucket is required")
	case accessKeyID == "":
		return nil, fmt.Errorf("access key id is required")
	case accessKeySecret == "":
		return nil, fmt.Errorf("access key secret is required")
	}
	cli, err := oss.New(withHTTPS(endpoint), accessKeyID, accessKeySecret)
	if err != nil {
		return nil, fmt.Errorf("create oss client: %w", err)
	}
	b, err := cli.Bucket(bucket)
	if err != nil {
		return nil, fmt.Errorf("open bucket %q: %w", bucket, err)
	}
	return &Client{bucket: b}, nil
}

// The two top-level directories this project uses inside the bucket.
//
// Everything the agent ever touches lives under Root, so its RAM policy is
// one path instead of the whole bucket. AdminRoot sits beside it rather than
// inside it: the employee roster is the one thing no agent may ever read, and
// keeping it physically outside the agent's directory is a stronger guarantee
// than carving an exception out of a prefix the agent otherwise has access to.
//
// Both binaries derive every key through the helpers below, so this is the
// only place the layout is written down -- agent and admin cannot drift onto
// different ones.
const (
	Root      = "agent_workdir/"
	AdminRoot = "admin/"
)

// FilePrefix is an administrator-managed staging area for files that need to
// reach a cloud desktop -- agent.exe itself, most often.
//
// It sits under AdminRoot, not under Root, because no agent ever reads it:
// the binary has to exist before the process that would download it does.
// Transfers happen through a signed URL instead, which needs no credentials
// on the receiving machine at all.
const FilePrefix = AdminRoot + "files/"

// FileKey is where one staged file lives. The name is reduced to a single
// path element so a crafted name cannot write elsewhere in the bucket.
func FileKey(name string) string {
	return FilePrefix + sanitiseSegment(name)
}

// FileFromKey recovers the file name from a staged-file key.
func FileFromKey(key string) string {
	return strings.TrimPrefix(key, FilePrefix)
}

// PolicyKey is the browser block policy, which is machine-wide rather than
// per-employee: it is written into HKLM and protects the whole box no matter
// who logs in. It therefore lives at the top of Root, reachable without
// knowing which employee a machine serves -- an unbound machine must still be
// locked down, and it has no employee directory to read from.
func PolicyKey() string {
	return Root + "policy.json"
}

// UserPrefix is one employee's directory: everything belonging to that
// employee lives under it, so a single prefix covers the whole person.
//
// The user name reaches us from the local machine, so it is reduced to a
// single path element: a value like "../other" must not let an agent reach
// another employee's directory.
func UserPrefix(user string) string {
	return Root + sanitiseSegment(user) + "/"
}

// UserKey builds the object key for one file in an employee's directory.
func UserKey(user, name string) string {
	return UserPrefix(user) + name
}

// DataCollectDir is where the agent's session collector writes, inside each
// employee's own directory rather than in one shared place. Access is
// write-only (see Putter) and the feature is off by default, gated by
// policy.collectEnabled -- see OSS布局.md §6.
const DataCollectDir = "data_collect/"

// DataCollectPrefix is one employee's collected-data directory.
func DataCollectPrefix(user string) string {
	return UserPrefix(user) + DataCollectDir
}

// DataCollectKey builds the object key for one collected session file inside
// an employee's data_collect directory. rel is the file's path relative to the
// employee profile (for example ".claude/projects/p/a.jsonl").
//
// rel is rooted and cleaned so a crafted "../" cannot climb out of the
// employee's directory; agents have write-only access here, and this keeps a
// bad relative path from landing an object anywhere else in the bucket.
func DataCollectKey(user, rel string) string {
	rel = strings.ReplaceAll(rel, `\`, "/")
	clean := strings.TrimPrefix(path.Clean("/"+rel), "/")
	return DataCollectPrefix(user) + clean
}

// DataCollectUser is the inverse of DataCollectKey: it reports which user a
// collected object belongs to and the path relative to that user's
// data_collect/ directory. ok is false for any key that is not under some
// user's data_collect/. It lets the admin enumerate collected data by user
// straight from object keys, without knowing the roster in advance -- which
// matters now that collection is machine-wide and can capture accounts that
// were never added to the roster.
func DataCollectUser(key string) (user, rel string, ok bool) {
	if !strings.HasPrefix(key, Root) {
		return "", "", false
	}
	rest := strings.TrimPrefix(key, Root) // {user}/data_collect/{rel}
	i := strings.IndexByte(rest, '/')
	if i <= 0 {
		return "", "", false
	}
	after := rest[i+1:] // data_collect/{rel}
	if !strings.HasPrefix(after, DataCollectDir) {
		return "", "", false
	}
	return rest[:i], strings.TrimPrefix(after, DataCollectDir), true
}

// AdminKey builds a key under the admin-only directory, which sits outside
// Root entirely. Agent credentials are not authorised for it, which keeps the
// roster out of their reach.
func AdminKey(name string) string {
	return AdminRoot + name
}

// The fixed prefixes inside Root.
const (
	BindingPrefix = Root + "_bindings/"
	StatusPrefix  = Root + "_status/"
	AgentPrefix   = Root + "_agent/"
)

// AgentBinaryKey is where the administrator stages a new agent binary for
// self-update. Agents read it (their RAM policy needs GetObject on
// _agent/*), verify its checksum against the policy, then replace themselves.
func AgentBinaryKey() string { return AgentPrefix + "agent.exe" }

// CodexPrefix holds the repackaged Codex desktop installers. Agents read it
// (their RAM policy needs GetObject on _codex/*); only the administrator
// writes here, via `admin codex publish`.
const CodexPrefix = Root + "_codex/"

// CodexInstallerKey is where one version of the Codex installer lives. Unlike
// AgentBinaryKey this is versioned rather than a single overwritten object:
// the installer is ~700 MB, so keeping past versions in place makes a rollback
// a policy edit instead of a re-upload.
func CodexInstallerKey(version string) string {
	return CodexPrefix + "codex-setup-" + sanitiseSegment(version) + ".exe"
}

// LogPrefix holds each machine's recent agent log, so the admin can read it
// without reaching the machine. Agents write here (RAM policy needs PutObject
// on _logs/*); the admin reads it.
const LogPrefix = Root + "_logs/"

// LogKey is where one machine's log tail is stored.
func LogKey(machine string) string { return LogPrefix + sanitiseSegment(machine) + ".log" }

// BindingKey is where an agent learns which employee its machine serves.
func BindingKey(machine string) string {
	return BindingPrefix + sanitiseSegment(machine) + ".json"
}

// StatusKey is where an agent reports what it did.
func StatusKey(machine string) string {
	return StatusPrefix + sanitiseSegment(machine) + ".json"
}

// MachineFromStatusKey recovers the machine name from a status object key,
// so the admin can list machines without a separate index.
func MachineFromStatusKey(key string) string {
	name := strings.TrimPrefix(key, StatusPrefix)
	return strings.TrimSuffix(name, ".json")
}

// MachineFromBindingKey recovers the machine name from a binding object key.
func MachineFromBindingKey(key string) string {
	name := strings.TrimPrefix(key, BindingPrefix)
	return strings.TrimSuffix(name, ".json")
}

// sanitiseSegment reduces a caller-supplied name to a single safe path element.
func sanitiseSegment(name string) string {
	normalised := strings.ReplaceAll(name, `\`, "/")
	clean := path.Base(path.Clean("/" + normalised))
	if clean == "/" || clean == "." {
		return "_invalid"
	}
	return clean
}

// Delete removes an object. Used when unbinding a machine.
func (c *Client) Delete(key string) error {
	if err := c.bucket.DeleteObject(key); err != nil {
		return fmt.Errorf("delete %q: %w", key, err)
	}
	return nil
}

// Get downloads an object and returns its bytes along with the ETag, which
// callers use to skip work when nothing changed.
func (c *Client) Get(key string) ([]byte, string, error) {
	meta, err := c.bucket.GetObjectDetailedMeta(key)
	if err != nil {
		return nil, "", fmt.Errorf("head %q: %w", key, classify(err))
	}
	etag := strings.Trim(meta.Get("Etag"), `"`)

	rc, err := c.bucket.GetObject(key)
	if err != nil {
		return nil, "", fmt.Errorf("get %q: %w", key, classify(err))
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, "", fmt.Errorf("read %q: %w", key, err)
	}
	return data, etag, nil
}

// Put uploads an object, replacing whatever was there.
func (c *Client) Put(key string, data []byte) error {
	if err := c.bucket.PutObject(key, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("put %q: %w", key, err)
	}
	return nil
}

// Head reports an object's ETag and whether it exists at all.
func (c *Client) Head(key string) (string, bool, error) {
	exists, err := c.bucket.IsObjectExist(key)
	if err != nil {
		return "", false, fmt.Errorf("stat %q: %w", key, err)
	}
	if !exists {
		return "", false, nil
	}
	meta, err := c.bucket.GetObjectDetailedMeta(key)
	if err != nil {
		return "", false, fmt.Errorf("head %q: %w", key, err)
	}
	return strings.Trim(meta.Get("Etag"), `"`), true, nil
}

// List returns every object key under a prefix, following pagination.
func (c *Client) List(prefix string) ([]string, error) {
	var keys []string
	marker := ""
	for {
		res, err := c.bucket.ListObjects(oss.Prefix(prefix), oss.Marker(marker))
		if err != nil {
			return nil, fmt.Errorf("list %q: %w", prefix, err)
		}
		for _, o := range res.Objects {
			keys = append(keys, o.Key)
		}
		if !res.IsTruncated {
			return keys, nil
		}
		marker = res.NextMarker
	}
}

// ObjectInfo is one object's listing metadata: enough to count and age
// collected files without downloading any of them.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// ListInfo is List with each object's size and modification time, following
// pagination the same way. The admin uses it to report collection activity
// (how many session files landed, and when the latest arrived) without ever
// reading the conversations themselves.
func (c *Client) ListInfo(prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	marker := ""
	for {
		res, err := c.bucket.ListObjects(oss.Prefix(prefix), oss.Marker(marker))
		if err != nil {
			return nil, fmt.Errorf("list %q: %w", prefix, err)
		}
		for _, o := range res.Objects {
			out = append(out, ObjectInfo{Key: o.Key, Size: o.Size, LastModified: o.LastModified})
		}
		if !res.IsTruncated {
			return out, nil
		}
		marker = res.NextMarker
	}
}

// Sentinel errors from Verify, so a caller (the admin's interactive prompt)
// can tell a wrong key from a valid key whose RAM policy is too narrow: the
// first is fixed by re-typing, the second by widening the policy.
var (
	ErrBadCredentials = errors.New("access key id or secret is wrong")
	ErrAccessDenied   = errors.New("credentials are valid but lack permission")
)

// Verify makes one minimal authenticated request to confirm the credentials
// actually work: it lists a single object at the bucket root, the smallest
// call that still exercises the signature. A nil return means the key can
// reach the bucket; ErrBadCredentials and ErrAccessDenied classify the two
// failures the caller reacts to differently.
func (c *Client) Verify() error {
	if _, err := c.bucket.ListObjects(oss.MaxKeys(1)); err != nil {
		return classifyVerify(err)
	}
	return nil
}

// classifyVerify maps the OSS auth failures onto the sentinels above. Anything
// else (a bad bucket, a network error) is passed through unchanged.
func classifyVerify(err error) error {
	var svc oss.ServiceError
	if errors.As(err, &svc) {
		switch svc.Code {
		case "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return fmt.Errorf("%w (%s)", ErrBadCredentials, svc.Code)
		case "AccessDenied":
			return fmt.Errorf("%w (%s)", ErrAccessDenied, svc.Code)
		}
	}
	return err
}

// ErrNotFound means the object definitively does not exist -- the service
// answered, and answered 404.
//
// Callers must be able to tell this apart from "we could not reach the
// store". Deleting an employee's credentials is how a revocation is signalled,
// and acting on a network error as though it were a deletion would wipe every
// machine's logins the first time the bucket was briefly unreachable.
var ErrNotFound = errors.New("object not found")

// classify turns an SDK error into ErrNotFound when, and only when, the
// service returned 404. Anything else is passed through unchanged.
func classify(err error) error {
	var svc oss.ServiceError
	if errors.As(err, &svc) && svc.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, svc.Code)
	}
	return err
}

// SignedURL returns a URL that grants temporary read access to one object
// without the holder needing any credentials.
//
// This is how a file reaches a cloud desktop: the administrator signs a link
// and the machine fetches it with any HTTP client. The signature is made with
// the administrator's key but never exposes it, and the link stops working
// when ttl elapses.
//
// The URL points at whatever endpoint this client was built with. The admin
// binary uses the public endpoint, so the link works from anywhere -- which
// is the point, since the machine downloading it has no OSS credentials.
func (c *Client) SignedURL(key string, ttl time.Duration) (string, error) {
	secs := int64(ttl / time.Second)
	if secs <= 0 {
		return "", fmt.Errorf("expiry must be positive, got %s", ttl)
	}
	url, err := c.bucket.SignURL(key, oss.HTTPGet, secs)
	if err != nil {
		return "", fmt.Errorf("sign url for %q: %w", key, err)
	}
	return url, nil
}
