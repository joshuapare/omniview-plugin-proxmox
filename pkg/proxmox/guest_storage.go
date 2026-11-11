package proxmox

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// guestConfigTTL is how long a guest's storage usage is reused before its
// config is read again. Proxmox has no bulk config endpoint and no change
// feed, so each refresh is one request per guest; disk changes show up
// within this interval, migrations at once.
const guestConfigTTL = time.Minute

// configReadConcurrency bounds parallel guest config reads.
const configReadConcurrency = 4

// volumeKey matches the guest config keys that hold a volume: QEMU disks,
// CD-ROM drives, EFI and TPM state, LXC root and mount points, and detached
// (unused) disks.
var volumeKey = regexp.MustCompile(`^(ide|sata|scsi|virtio|mp|unused)\d+$|^(efidisk0|tpmstate0|rootfs)$`)

// Guest identifies a QEMU or LXC guest for GuestStorage.
type Guest struct {
	// ID is the cluster resource ID, qemu/100 or lxc/200.
	ID   string
	Node string
}

type guestStorage struct {
	node    string
	storage []string
	readAt  time.Time
}

// guestStorageCache keeps each guest's storage usage between reads.
type guestStorageCache struct {
	mu     sync.Mutex
	guests map[string]guestStorage
}

// GuestStorage returns the storage IDs (storage.cfg names, such as
// local-lvm) holding each guest's volumes, keyed by guest ID. It reads a
// guest's config when it has none cached, the guest moved to another node,
// or the cached one is older than guestConfigTTL. A guest whose config
// cannot be read keeps its previous result, or is absent from the map when
// there is none. Cached guests of the listed types (qemu, lxc) that are not
// in guests are forgotten.
func (c *Client) GuestStorage(ctx context.Context, guests []Guest) map[string][]string {
	ctx, span := Tracer().Start(ctx, "proxmox.guest_storage")
	lockStart := time.Now()
	c.storage.mu.Lock()
	lockWait := time.Since(lockStart)
	defer c.storage.mu.Unlock()
	defer func() {
		span.End()
	}()
	if c.storage.guests == nil {
		c.storage.guests = map[string]guestStorage{}
	}

	now := time.Now()
	var stale []Guest
	for _, g := range guests {
		cached, ok := c.storage.guests[g.ID]
		if !ok || cached.node != g.Node || now.Sub(cached.readAt) >= guestConfigTTL {
			stale = append(stale, g)
		}
	}

	misses, hits := len(stale), len(guests)-len(stale)
	span.SetAttributes(
		attribute.String("proxmox.cluster_id", c.ClusterID),
		attribute.Int("guests", len(guests)),
		attribute.Int("cache_hits", hits),
		attribute.Int("cache_misses", misses),
		attribute.Int64("lock_wait_ms", durationMs(lockWait)),
	)

	read := make([]*guestStorage, len(stale))
	sem := make(chan struct{}, configReadConcurrency)
	var wg sync.WaitGroup
	for i, g := range stale {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			storage, err := c.guestConfigStorage(ctx, g)
			if err == nil {
				read[i] = &guestStorage{node: g.Node, storage: storage, readAt: now}
				return
			}
			// The guest keeps its previous result (or none); a failed config
			// read is tolerated.
			pkgLogger().Debugw(ctx, "proxmox guest config read failed",
				append([]any{"cluster_id", c.ClusterID, "node", g.Node}, classFields(err)...)...)
		}()
	}
	wg.Wait()
	for i, g := range stale {
		if read[i] != nil {
			c.storage.guests[g.ID] = *read[i]
		}
	}

	out := make(map[string][]string, len(guests))
	listed := map[string]bool{}
	for _, g := range guests {
		typ, _, _ := strings.Cut(g.ID, "/")
		listed[typ] = true
		if cached, ok := c.storage.guests[g.ID]; ok {
			out[g.ID] = cached.storage
		}
	}
	for id := range c.storage.guests {
		typ, _, _ := strings.Cut(id, "/")
		if _, ok := out[id]; !ok && listed[typ] {
			delete(c.storage.guests, id)
		}
	}
	return out
}

// guestConfigStorage reads GET /nodes/{node}/{qemu|lxc}/{vmid}/config and
// returns the sorted, distinct storage IDs its volumes are on.
func (c *Client) guestConfigStorage(ctx context.Context, g Guest) ([]string, error) {
	typ, vmid, _ := strings.Cut(g.ID, "/")
	var config map[string]any
	path := "/nodes/" + url.PathEscape(g.Node) + "/" + url.PathEscape(typ) + "/" + url.PathEscape(vmid) + "/config"
	if err := c.get(ctx, path, nil, &config); err != nil {
		return nil, err
	}
	storage := []string{}
	for key, value := range config {
		s, ok := value.(string)
		if !ok || !volumeKey.MatchString(key) {
			continue
		}
		if id := volumeStorage(s); id != "" && !slices.Contains(storage, id) {
			storage = append(storage, id)
		}
	}
	slices.Sort(storage)
	return storage, nil
}

// volumeStorage returns the storage ID of a config volume value such as
// `local-lvm:vm-100-disk-0,size=32G` or `local:iso/debian.iso,media=cdrom`.
// Values without a storage volume return "": an empty CD-ROM (`none`,
// `cdrom`), a passed-through device or a bind mount (`/dev/...`, `/mnt/...`).
func volumeStorage(value string) string {
	volume, _, _ := strings.Cut(value, ",")
	if strings.HasPrefix(volume, "/") {
		return ""
	}
	storage, _, ok := strings.Cut(volume, ":")
	if !ok {
		return ""
	}
	return storage
}
