package adminweb

import (
	"log"
	"strings"
	"sync"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/ecdclient"
)

// cloudLookup answers "is this machine asleep or has it died", by asking the
// platform -- but only when the machine's own report leaves that open.
//
// Nothing here polls. A fleet that is reporting normally needs no cloud calls
// at all: the question only arises for a machine that has gone quiet, so the
// lookup happens then and not on a timer.
type cloudLookup struct {
	client *ecdclient.Client
	ttl    time.Duration

	mu        sync.Mutex
	desktops  map[string]admincore.CloudDesktop // keyed by lowercased host name
	fetchedAt time.Time
	lastErr   error
}

// cloudCacheTTL is how long one answer serves.
//
// The page reloads itself while a publish runs, and an operator watching a
// machine come back will reload too, so without a cache a single quiet machine
// would turn every refresh into an API call. A minute is far shorter than the
// ten minutes it takes to be called quiet in the first place, so nothing is
// stale in a way that matters.
const cloudCacheTTL = time.Minute

func newCloudLookup(client *ecdclient.Client) *cloudLookup {
	if client == nil {
		return nil
	}
	return &cloudLookup{client: client, ttl: cloudCacheTTL}
}

// annotate fills in cloud state for the machines that need it, in place.
//
// A failure is logged and otherwise ignored: the console has to keep working
// when the platform does not answer, and an un-annotated machine simply falls
// back to the reading its own report supports.
func (c *cloudLookup) annotate(machines []admincore.MachineState) {
	if c == nil {
		return
	}
	needed := false
	for i := range machines {
		if machines[i].NeedsCloudLookup() {
			needed = true
			break
		}
	}
	if !needed {
		return
	}

	desktops, err := c.desktopsByHost()
	if err != nil {
		log.Printf("adminweb: cloud lookup: %v", err)
		return
	}
	for i := range machines {
		if !machines[i].NeedsCloudLookup() {
			continue
		}
		d, ok := desktops[strings.ToLower(machines[i].Machine)]
		if !ok {
			// Queried and matched nothing: the desktop is gone, which is
			// itself worth saying rather than leaving as plain silence.
			d = admincore.CloudDesktop{Found: false}
		}
		cd := d
		machines[i].Cloud = &cd
	}
}

func (c *cloudLookup) desktopsByHost() (map[string]admincore.CloudDesktop, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.desktops != nil && time.Since(c.fetchedAt) < c.ttl {
		return c.desktops, c.lastErr
	}
	list, err := c.client.DescribeDesktops()
	c.fetchedAt = time.Now()
	if err != nil {
		c.lastErr = err
		return nil, err
	}
	byHost := make(map[string]admincore.CloudDesktop, len(list))
	for _, d := range list {
		if d.HostName == "" {
			continue // nothing to match an agent report against
		}
		byHost[strings.ToLower(d.HostName)] = admincore.CloudDesktop{
			DesktopID:  d.DesktopID,
			Status:     d.Status,
			Hibernated: d.Hibernated(),
			Found:      true,
		}
	}
	c.desktops, c.lastErr = byHost, nil
	return byHost, nil
}
